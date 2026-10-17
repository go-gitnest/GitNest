package references

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The PostgreSQL side of references: recording what a text named, and reading what a text was named by.
//
// # Why recording is a replace rather than an insert
//
// A text's references are **derived from its current content**, so re-saving an issue body or editing a
// comment produces a new set that may differ from the old one in both directions. `Record` therefore
// deletes the source's rows and inserts the new set **in one transaction**, which is what makes the table
// a statement about what a text says now rather than an accumulation of everything it has ever said. An
// insert-only design would leave a reference behind after the words naming it were edited away, and the
// target's timeline would keep showing a link nobody can find.
//
// The unique constraint on the source/target pair is still worth having, because it also refuses a
// duplicate within one recorded set — a text that says `#5` twice still records one relationship.

// Sources persist the references a text makes.
type Sources struct {
	db *sql.DB
}

// NewSources returns a store over a database handle.
func NewSources(db *sql.DB) *Sources {
	return &Sources{db: db}
}

// RecordInput is the set of references one text makes.
type RecordInput struct {
	// SourceIssueID is the issue whose text was parsed, and it is always set: a comment's reference is
	// also the issue's, which is what lets the outgoing timeline be read per issue in one query.
	SourceIssueID string
	// SourceCommentID is set when the text is a comment's rather than the issue body's.
	SourceCommentID string
	// References are the targets the text named, already extracted.
	References []Reference
	// Written is when the text was written, which is what the row's `created_at` takes. It is passed in
	// rather than defaulted so that a reference recorded by a later re-parse still belongs where it
	// appeared in the conversation.
	Written time.Time
}

// Record replaces the references a text makes.
//
// The delete and the insert share one transaction, so a failure leaves the previous set intact rather
// than the source with none. That matters because the caller's read path shows an empty reference list
// as "no references", which would be a lie about a text that still contains them.
func (s *Sources) Record(ctx context.Context, input RecordInput) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recording references: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	deleteStatement := `DELETE FROM issue_references WHERE source_issue_id = $1`
	args := []any{input.SourceIssueID}
	if input.SourceCommentID != "" {
		// A comment's rows are removed by the comment, not by the issue — otherwise editing one comment
		// would wipe every other comment's references on the same issue.
		deleteStatement += ` AND source_comment_id = $2`
		args = append(args, input.SourceCommentID)
	} else {
		// The body's rows are the ones with no comment, which is why the null test is explicit: `= NULL`
		// matches nothing.
		deleteStatement += ` AND source_comment_id IS NULL`
	}
	if _, err := tx.ExecContext(ctx, deleteStatement, args...); err != nil {
		return fmt.Errorf("clear the previous references: %w", err)
	}

	for _, reference := range input.References {
		// The target repository is resolved by the caller, because `#1` names a repository the parser
		// does not know. A reference whose target repository is empty is a same-repository one with no
		// repository to attach to, which is a caller's bug rather than a row to write.
		if reference.TargetRepositoryID == "" {
			continue
		}
		// **A self-reference is skipped here, and this is where it can be decided.** The migration records
		// why a `CHECK` cannot express it: the source is an identifier in this table while the target is a
		// `(repository, number)` pair, so the comparison needs a join. The caller holds both facts, so it
		// tells us by leaving the identifier empty for a reference that resolves to its own source.
		if reference.SelfTarget {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO issue_references
				(id, source_issue_id, source_comment_id, target_repository_id, target_number, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING`,
			newIdentifier(), input.SourceIssueID, nullString(input.SourceCommentID),
			reference.TargetRepositoryID, reference.Number, input.Written); err != nil {
			return fmt.Errorf("record a reference to %s: %w", reference.String(), err)
		}
	}
	return tx.Commit()
}

// Incoming is a reference made *to* a target, which is what the target's timeline shows.
type Incoming struct {
	// SourceRepositoryID and SourceNumber identify the text that made the reference, which is what a
	// timeline entry links back to.
	SourceRepositoryID string
	SourceOwner        string
	SourceRepository   string
	SourceNumber       int64
	// SourceCommentID is set when the referencing text was a comment rather than an issue body.
	SourceCommentID string
	// CreatedAt is when the referencing text was written.
	CreatedAt time.Time
}

// Incoming returns the references made to one target, newest first.
//
// **It joins the source issue so a timeline entry can link back and name its repository**, which is the
// whole point of showing a cross-reference: a reader who sees "referenced by #7" needs to be able to reach
// issue 7.
//
// The read is scoped to the target pair, which the `issue_references_target_idx` index serves. A caller
// checks the reader's access before calling this, so nothing here decides visibility — the rule that this
// package never makes an access decision.
func (s *Sources) Incoming(ctx context.Context, repositoryID string, number int64) ([]Incoming, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source.repository_id,
		       COALESCE(owner.username, ''),
		       source_repository.name,
		       source.number,
		       COALESCE(reference.source_comment_id::text, ''),
		       reference.created_at
		FROM issue_references AS reference
		JOIN issues AS source ON source.id = reference.source_issue_id
		JOIN repositories AS source_repository ON source_repository.id = source.repository_id
		LEFT JOIN users AS owner ON owner.id = source_repository.owner_id
		WHERE reference.target_repository_id = $1 AND reference.target_number = $2
		ORDER BY reference.created_at DESC, reference.id DESC`, repositoryID, number)
	if err != nil {
		return nil, fmt.Errorf("read the incoming references: %w", err)
	}
	defer func() { _ = rows.Close() }()

	found := []Incoming{}
	for rows.Next() {
		var entry Incoming
		if err := rows.Scan(&entry.SourceRepositoryID, &entry.SourceOwner,
			&entry.SourceRepository, &entry.SourceNumber, &entry.SourceCommentID,
			&entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("read an incoming reference: %w", err)
		}
		found = append(found, entry)
	}
	return found, rows.Err()
}

// Outgoing is a reference a text made, resolved to what a timeline shows.
type Outgoing struct {
	// TargetRepositoryID and TargetNumber identify the target.
	TargetRepositoryID string
	TargetOwner        string
	TargetRepository   string
	TargetNumber       int64
	// SourceCommentID is set when the reference came from a comment.
	SourceCommentID string
	CreatedAt       time.Time
}

// Outgoing returns the references one issue's text makes, newest first.
//
// It joins the target repository so a timeline entry can link and name it, and it does **not** filter by
// visibility: the caller resolves each target through the shared decision, because visibility is per
// reader and this query is not.
func (s *Sources) Outgoing(ctx context.Context, sourceIssueID string) ([]Outgoing, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_repository.id,
		       COALESCE(owner.username, ''),
		       target_repository.name,
		       reference.target_number,
		       COALESCE(reference.source_comment_id::text, ''),
		       reference.created_at
		FROM issue_references AS reference
		JOIN repositories AS target_repository ON target_repository.id = reference.target_repository_id
		LEFT JOIN users AS owner ON owner.id = target_repository.owner_id
		WHERE reference.source_issue_id = $1
		ORDER BY reference.created_at DESC, reference.id DESC`, sourceIssueID)
	if err != nil {
		return nil, fmt.Errorf("read the outgoing references: %w", err)
	}
	defer func() { _ = rows.Close() }()

	found := []Outgoing{}
	for rows.Next() {
		var entry Outgoing
		if err := rows.Scan(&entry.TargetRepositoryID, &entry.TargetOwner,
			&entry.TargetRepository, &entry.TargetNumber, &entry.SourceCommentID,
			&entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("read an outgoing reference: %w", err)
		}
		found = append(found, entry)
	}
	return found, rows.Err()
}

// nullString renders an empty string as SQL NULL, because the source comment is nullable and `= NULL`
// matches nothing.
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// newIdentifier returns an identifier for a recorded reference.
//
// UUIDv7 rather than v4, per the data-model rule that an identifier is generated in Go as a time-ordered
// UUIDv7 so index inserts stay local.
func newIdentifier() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
