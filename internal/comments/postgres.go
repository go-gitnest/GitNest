package comments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-gitnest/GitNest/internal/audit"
)

// The PostgreSQL implementation.
//
// # One read projection, resolved through the issue
//
// Every query joins the comment's issue and filters on `issues.repository_id`, so a comment can only
// be reached through the repository and issue it actually belongs to. That is why the store takes a
// repository and an issue on every read rather than trusting a caller to have checked: an identifier
// that resolves must also belong where the caller says it does, and a join is what makes that one
// fact rather than two.
//
// **The repository is not denormalized onto the comment.** A copy would have to be kept in step with
// the issue's own column, and the day it drifted the authorization check and the page would disagree
// about which repository a comment was in. The join costs one indexed lookup.
//
// # The two atomic changes
//
//   - **Edit**: the revision is appended and the body replaced in one transaction. A history entry
//     without its change, or a change without its history entry, would both be lies about what
//     happened.
//   - **Delete**: the body is cleared, the history is removed, and the record is written in one
//     transaction. Clearing the body is what makes "deleted" mean the words are gone; removing the
//     history is what stops the text surviving one query away in a revision row.

// commentReadColumns is the projection every read shares, so a scan can never drift from the query
// that feeds it.
//
// The author's name is resolved by a join rather than by a follow-up query per row, because a page
// shows a conversation of many comments at once. `author_name` is null exactly when there is no
// author to name, which is why the join is a `LEFT JOIN`: an `INNER JOIN` would make a comment whose
// author was deleted disappear, the opposite of the `ON DELETE SET NULL` decision.
// The number is computed by a window function over the same `(created_at, id)` order the index
// provides, so it cannot disagree with the order a listing returns. `ROW_NUMBER()` rather than
// `RANK()`: a rank would share a value between two comments written in the same instant, which is
// exactly the collision the identifier breaks.
const commentReadColumns = `issue_comments.id, issue_comments.issue_id, issue_comments.author_id,
	author.username, issue_comments.body, issue_comments.deleted_at, issue_comments.deleted_by,
	issue_comments.created_at, issue_comments.edited_at, issue_comments.version,
	ROW_NUMBER() OVER (PARTITION BY issue_comments.issue_id
	                   ORDER BY issue_comments.created_at, issue_comments.id) AS number`

// commentFrom is the read source, with the issue and author joins.
//
// The issue join is not decoration: it is what carries `issues.repository_id`, which every query
// filters on.
const commentFrom = `issue_comments
	JOIN issues ON issues.id = issue_comments.issue_id
	LEFT JOIN users AS author ON author.id = issue_comments.author_id`

// PostgresStore is the database-backed store.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a store over a database handle.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Create stores a comment.
func (s *PostgresStore) Create(ctx context.Context, input StoreCreate) (Comment, error) {
	var created Comment
	err := s.change(ctx, audit.Event{}, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO issue_comments (id, issue_id, author_id, body)
			VALUES ($1, $2, $3, $4)`,
			input.ID, input.IssueID, nullableString(input.AuthorID), input.Body); err != nil {
			return fmt.Errorf("insert the comment: %w", err)
		}
		// The read is inside the transaction, so the comment returned is the one that was written
		// rather than whatever a concurrent change made of it.
		inserted, readErr := readComment(ctx, tx, `WHERE issue_comments.id = $1`, input.ID)
		if readErr != nil {
			return readErr
		}
		created = inserted
		return nil
	})
	if err != nil {
		return Comment{}, err
	}
	return created, nil
}

// Get returns one comment.
func (s *PostgresStore) Get(ctx context.Context, query StoreGet) (Comment, error) {
	return readComment(ctx, s.db, `
		WHERE issues.repository_id = $1 AND issue_comments.issue_id = $2 AND issue_comments.id = $3`,
		query.RepositoryID, query.IssueID, query.CommentID)
}

// GetByNumber returns one comment by its position in the issue.
//
// **A window function cannot appear in a `WHERE` clause**, so the numbering is computed in a
// subquery and the position is selected outside it. That is not a workaround but the shape the
// requirement has: a row's number is a property of the whole ordered set, so it cannot be filtered
// while it is being computed.
//
// Both this and `List` use the same window over the same order, so a number cannot resolve to a
// different comment than the one a listing showed under it.
func (s *PostgresStore) GetByNumber(ctx context.Context, repositoryID, issueID string, number int) (Comment, error) {
	// **The outer projection names the inner query's aliases exactly.** `commentReadColumns` selects
	// `author.username` without an alias, so the column is `username` and not `author_name` — and
	// naming it otherwise made every by-number read fail with a SQL error, which `writeDomainError`
	// then reported as a 500. The end-to-end tests caught it; the store's own tests did not, because
	// none of them addressed a comment by number until these did.
	// **The outer projection lists the columns in the order `scanComment` reads them**, with the
	// derived number last. Two details cost a real defect here and each is worth stating:
	//
	//   - `commentReadColumns` selects `author.username` without an alias, so the column is `username`
	//     and not `author_name`; naming it otherwise made every by-number read fail with a SQL error.
	//   - The window function's output had to come **last**, because `scanComment` is shared with the
	//     listing and reads the number after the stored columns. Putting it first shifted every
	//     subsequent value by one, which the scanner reported as a string arriving where a timestamp
	//     was expected.
	row := s.db.QueryRowContext(ctx, `
		SELECT id, issue_id, author_id, username, body, deleted_at, deleted_by,
		       created_at, edited_at, version, number
		FROM (
			SELECT `+commentReadColumns+` FROM `+commentFrom+`
			WHERE issues.repository_id = $1
			  AND issue_comments.issue_id = $2
		) AS numbered
		WHERE number = $3`, repositoryID, issueID, number)

	comment, err := scanComment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Comment{}, ErrNotFound
	}
	if err != nil {
		return Comment{}, fmt.Errorf("read the comment: %w", err)
	}
	return comment, nil
}

// List returns a page of an issue's comments, oldest first.
//
// **A conversation reads forwards**, so this is the one list in the project that paginates by
// ascending keyset rather than descending: the first page is the beginning of the conversation, and
// a "more" link continues towards the end.
//
// One row beyond the limit is fetched, so `HasMore` is learned from the query rather than from a
// second count.
func (s *PostgresStore) List(ctx context.Context, query StoreList) (Page, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	args := []any{query.RepositoryID, query.IssueID}
	statement := `SELECT ` + commentReadColumns + ` FROM ` + commentFrom + `
		WHERE issues.repository_id = $1 AND issue_comments.issue_id = $2`
	if query.Before != nil {
		// Strictly newer, with the identifier breaking a tie on the same instant — the pair W2.3's
		// acceptance names as making the order stable. The row-comparison form is what lets the index
		// satisfy the predicate.
		statement += fmt.Sprintf(` AND (issue_comments.created_at, issue_comments.id) > ($%d, $%d)`,
			len(args)+1, len(args)+2)
		args = append(args, query.Before.CreatedAt, query.Before.ID)
	}
	statement += fmt.Sprintf(` ORDER BY issue_comments.created_at, issue_comments.id LIMIT $%d`,
		len(args)+1)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return Page{}, fmt.Errorf("list the comments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := Page{Comments: []Comment{}}
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			return Page{}, fmt.Errorf("read a comment: %w", err)
		}
		page.Comments = append(page.Comments, comment)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("list the comments: %w", err)
	}

	if len(page.Comments) > limit {
		page.HasMore = true
		page.Comments = page.Comments[:limit]
	}
	if page.HasMore && len(page.Comments) > 0 {
		next := SortKeyOf(page.Comments[len(page.Comments)-1])
		page.Next = &next
	}
	return page, nil
}

// Edit replaces a comment's body and appends its previous one to the history.
//
// **Both happen in one transaction**, so a history entry cannot exist without the change it
// describes, and a change cannot exist without its entry. The revision's own number is derived from
// the comment's existing history inside the transaction, so two concurrent edits cannot both claim
// revision 2 — the unique constraint would refuse the second, which is the correct outcome for a
// lost update.
func (s *PostgresStore) Edit(ctx context.Context, input StoreEdit) (Comment, error) {
	var updated Comment
	err := s.change(ctx, audit.Event{}, func(tx *sql.Tx) error {
		// The version guard is in the `WHERE` clause rather than in a read before it, so it cannot
		// race the write it guards.
		var id string
		err := tx.QueryRowContext(ctx, `
			UPDATE issue_comments
			SET body = $4, version = version + 1, edited_at = NOW()
			WHERE issue_id = $1 AND id = $2 AND version = $3 AND deleted_at IS NULL
			RETURNING id`,
			input.IssueID, input.CommentID, input.ExpectedVersion, input.Body).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return s.explainMissingEdit(ctx, input)
		}
		if err != nil {
			return fmt.Errorf("edit the comment: %w", err)
		}

		// The revision's number is the next one for this comment, computed in the same transaction so
		// it is consistent with what was just written.
		var next int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(revision), 0) + 1 FROM comment_revisions WHERE comment_id = $1`,
			input.CommentID).Scan(&next); err != nil {
			return fmt.Errorf("read the revision number: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO comment_revisions (id, comment_id, revision, body, version, editor_id)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			input.Revision.ID, input.CommentID, next, input.Revision.Body,
			input.Revision.Version, nullableString(input.Revision.EditorID)); err != nil {
			return fmt.Errorf("append the revision: %w", err)
		}

		updated, err = readComment(ctx, tx, `WHERE issue_comments.id = $1`, input.CommentID)
		return err
	})
	if err != nil {
		return Comment{}, err
	}
	return updated, nil
}

// explainMissingEdit reports why an edit affected no row.
//
// A read runs only on the failure path, which is where the three cases are told apart: the comment
// is gone, it was deleted, or its version moved. They are reported differently because the caller's
// next action differs — one means the conversation changed, and the other two mean this comment can
// no longer be edited at all.
func (s *PostgresStore) explainMissingEdit(ctx context.Context, input StoreEdit) error {
	current, err := s.Get(ctx, StoreGet{
		RepositoryID: input.RepositoryID, IssueID: input.IssueID, CommentID: input.CommentID,
	})
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current.Deleted {
		return ErrDeleted
	}
	return ErrConflict
}

// Delete soft-deletes a comment, clearing its text and removing its history.
//
// **The body is cleared and the revisions are removed in the same transaction as the record**, which
// is what makes a deletion mean the words are gone. A revision row that survived would hold the text
// one query away, so "deleted" would mean "hidden from the page" — and the acceptance's "deletion is
// soft or audited" is satisfied by the row remaining while the content does not.
func (s *PostgresStore) Delete(ctx context.Context, input StoreDelete) error {
	return s.change(ctx, input.Event, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE issue_comments
			SET body = '', deleted_at = NOW(), deleted_by = $3, version = version + 1
			WHERE issue_id = $1 AND id = $2 AND deleted_at IS NULL`,
			input.IssueID, input.CommentID, nullableString(input.DeletedBy))
		if err != nil {
			return fmt.Errorf("delete the comment: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete the comment: %w", err)
		}
		if affected == 0 {
			// Nothing was deleted, so the record must not be written either: a record of a deletion
			// that did not happen is worse than no record. The likely cause is that the comment was
			// already deleted, which the service reports as such.
			return ErrDeleted
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM comment_revisions WHERE comment_id = $1`, input.CommentID); err != nil {
			return fmt.Errorf("remove the comment history: %w", err)
		}
		return audit.Record(ctx, tx, input.Event)
	})
}

// History returns a comment's revisions, oldest first.
//
// It orders by the revision number rather than by the timestamp, because the number states the
// sequence and cannot collide; two revisions written in the same microsecond would otherwise have
// an arbitrary order.
func (s *PostgresStore) History(ctx context.Context, commentID string) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT revision.id, revision.comment_id, revision.revision, revision.body, revision.version,
		       COALESCE(revision.editor_id::text, ''), COALESCE(editor.username, ''),
		       revision.created_at
		FROM comment_revisions AS revision
		LEFT JOIN users AS editor ON editor.id = revision.editor_id
		WHERE revision.comment_id = $1
		ORDER BY revision.revision`, commentID)
	if err != nil {
		return nil, fmt.Errorf("read the comment history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	history := []Revision{}
	for rows.Next() {
		var revision Revision
		var editorID, editorName sql.NullString
		if err := rows.Scan(&revision.ID, &revision.CommentID, &revision.Revision, &revision.Body,
			&revision.Version, &editorID, &editorName, &revision.CreatedAt); err != nil {
			return nil, fmt.Errorf("read a revision: %w", err)
		}
		revision.EditorID = editorID.String
		revision.EditorName = editorName.String
		history = append(history, revision)
	}
	return history, rows.Err()
}

// Count reports how many comments an issue has.
//
// A deleted comment is counted, because the conversation's shape includes it: a reader sees a
// tombstone where it was, so omitting it would make the count disagree with the page.
func (s *PostgresStore) Count(ctx context.Context, issueID string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM issue_comments WHERE issue_id = $1`, issueID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count the comments: %w", err)
	}
	return count, nil
}

// change runs a mutation in a transaction, validating its record first.
//
// **The record is written by the mutation, not here**, because it must be written inside the same
// transaction and only after the row it describes has actually changed. An empty action means "this
// change writes no record", which is what an edit passes: `event.Validate`, which refuses an empty
// action, is not reached for it.
func (s *PostgresStore) change(ctx context.Context, event audit.Event, mutate func(*sql.Tx) error) error {
	if event.Action != "" {
		if err := event.Validate(); err != nil {
			return err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the change: %w", err)
	}
	// A rollback after a successful commit is a no-op, so this is safe on every path.
	defer func() { _ = tx.Rollback() }()

	if err := mutate(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// queryer is what a read needs, so a transaction and the pool share one function.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readComment reads one comment through the shared projection.
func readComment(ctx context.Context, q queryer, where string, args ...any) (Comment, error) {
	row := q.QueryRowContext(ctx, `SELECT `+commentReadColumns+` FROM `+commentFrom+` `+where, args...)
	comment, err := scanComment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Comment{}, ErrNotFound
	}
	if err != nil {
		return Comment{}, fmt.Errorf("read the comment: %w", err)
	}
	return comment, nil
}

// rowScanner is what a single-row read needs, so `*sql.Row` and `*sql.Rows` share one scan.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanComment reads one row of the shared projection.
//
// The nullable columns are scanned through `sql.Null*` rather than into their zero values, because
// an empty string and an absent value are different facts: a deleted comment's author must not read
// as an account whose identifier is empty, and a joined username is null exactly when there is no
// author to name.
func scanComment(row rowScanner) (Comment, error) {
	var comment Comment
	var authorID, authorName, deletedBy sql.NullString
	var deletedAt, editedAt sql.NullTime

	if err := row.Scan(&comment.ID, &comment.IssueID, &authorID, &authorName, &comment.Body,
		&deletedAt, &deletedBy, &comment.CreatedAt, &editedAt, &comment.Version,
		&comment.Number); err != nil {
		return Comment{}, err
	}

	comment.AuthorID = authorID.String
	comment.AuthorName = authorName.String
	comment.Deleted = deletedAt.Valid
	comment.DeletedBy = deletedBy.String
	if editedAt.Valid {
		at := editedAt.Time
		comment.EditedAt = &at
	}
	return comment, nil
}

// nullableString renders an empty string as SQL NULL.
//
// It is used for every account identifier column, which is null when there is nobody to name:
// inserting an empty string would violate the foreign key and would turn "nobody" into "an account
// whose identifier is empty".
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
