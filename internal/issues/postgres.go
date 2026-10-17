package issues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-gitnest/GitNest/internal/audit"
)

// The PostgreSQL implementation.
//
// # The number allocation, which is the part worth reading
//
// `Create` runs three statements in one transaction, and the first is the one that makes the
// numbering correct:
//
//	UPDATE repositories SET next_issue_number = next_issue_number + 1
//	WHERE id = $1 RETURNING next_issue_number - 1
//
// Three properties follow, and all three are required:
//
//   - **Two concurrent creates never share a number.** The `UPDATE` takes the repository row's
//     lock, so the second creator waits on the first rather than reading the same value. This
//     is the data-model rule "Atomic `UPDATE ... SET n = n + 1`, never read-modify-write".
//   - **A failed transaction leaves no gap.** The increment is part of the transaction, so a
//     rollback undoes it and the number is handed out again. A `SEQUENCE` cannot do this:
//     `nextval` is not transactional, which is why the counter is a column.
//   - **The number is shared with pull requests.** It lives on the repository rather than on
//     this table, so phase 3's pull requests allocate from the same counter without a second
//     scheme to reconcile — which is what `data-model.md` requires when it says the two "share
//     one per-repository sequence".
//
// The cost is that issue creation serializes per repository. That is the right granularity: two
// repositories never contend, and one repository's issue creation is not a hot path.
//
// # One read projection, and why a write re-reads
//
// The read projection joins the author's and the closer's usernames, because a list page shows
// thirty issues at once and resolving thirty names afterwards is thirty queries.
//
// **A write therefore does not return the row it wrote.** An `INSERT ... RETURNING` cannot carry
// a join, so `Create`, `Edit`, and `SetState` return the identifier and then read it through the
// same projection every other read uses. That costs one indexed query inside the transaction
// that is already open, and it buys a single projection: two shapes — one for writes and one for
// reads — would be two things to keep in step, and the one that drifted would do so silently.
//
// # Every mutation writes its record in the same transaction
//
// The rule from `docs/reference/events.md` — "a failed audit write fails the operation" — is
// implemented by `change`, which wraps the mutation and its record in one transaction. The
// interface makes it the only shape available: a method that took the change alone would force
// the record to be written afterwards.

// issueReadColumns is the projection every read shares, so a scan can never drift from the
// query that feeds it.
//
// The two names are resolved by joins rather than by a follow-up query per row. `author_name`
// is empty when the author's account was deleted, which is exactly what a `LEFT JOIN` produces
// and what the nullable scan expects — an `INNER JOIN` would instead make the issue disappear,
// which would be the opposite of the `ON DELETE SET NULL` decision the migration records.
const issueReadColumns = `issues.id, issues.repository_id, issues.number, issues.title,
	issues.body, issues.author_id, author.username, issues.state, issues.closed_at,
	issues.closed_by, closer.username, issues.created_at, issues.updated_at, issues.version`

// issueFrom is the read query's source, with the two name joins.
const issueFrom = `issues
	LEFT JOIN users AS author ON author.id = issues.author_id
	LEFT JOIN users AS closer ON closer.id = issues.closed_by`

// PostgresStore is the database-backed store.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a store over a database handle.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Create allocates a number and stores the issue.
func (s *PostgresStore) Create(ctx context.Context, input StoreCreate) (Issue, error) {
	var created Issue
	err := s.change(ctx, input.Event, func(tx *sql.Tx) error {
		// The allocation. `next_issue_number - 1` is the number handed out, because the column
		// is the *next* value to use: `RETURNING` reports the row after the update.
		var number int64
		err := tx.QueryRowContext(ctx, `
			UPDATE repositories SET next_issue_number = next_issue_number + 1
			WHERE id = $1
			RETURNING next_issue_number - 1`, input.RepositoryID).Scan(&number)
		if errors.Is(err, sql.ErrNoRows) {
			// The repository was deleted between the service's access check and this
			// transaction. The service already decided the caller may read it, so this is a
			// disappearance rather than a denial.
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("allocate the issue number: %w", err)
		}

		// The record carries the number, which only this statement knows. Mutating the
		// caller's event here rather than requiring it in advance is what lets the number be
		// allocated inside the transaction that needs it.
		if input.Event.Detail == nil {
			input.Event.Detail = map[string]any{}
		}
		input.Event.Detail["number"] = number

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO issues (id, repository_id, number, title, body, author_id, state)
			VALUES ($1, $2, $3, $4, $5, $6, 'open')`,
			input.ID, input.RepositoryID, number, input.Title, input.Body,
			nullableString(input.AuthorID)); err != nil {
			return fmt.Errorf("insert the issue: %w", err)
		}

		created, err = readIssue(ctx, tx, `WHERE issues.id = $1`, input.ID)
		if err != nil {
			return fmt.Errorf("read the inserted issue: %w", err)
		}
		return audit.Record(ctx, tx, input.Event)
	})
	if err != nil {
		return Issue{}, err
	}
	return created, nil
}

// Get returns one issue by repository and number.
func (s *PostgresStore) Get(ctx context.Context, repositoryID string, number int64) (Issue, error) {
	return readIssue(ctx, s.db, `WHERE issues.repository_id = $1 AND issues.number = $2`,
		repositoryID, number)
}

// List returns a page, newest first, keyset-paginated on (created_at, id).
//
// One row beyond the limit is fetched, so `HasMore` is learned from the query rather than from
// a second count — and a page costs the same at any depth, which is the point of keyset
// pagination.
func (s *PostgresStore) List(ctx context.Context, query StoreList) (Page, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	// The predicate and the ordering are built as one unit, because a keyset page is wrong
	// unless the filter matches the sort exactly: a mismatch silently drops or repeats rows at
	// a page boundary.
	args := []any{query.RepositoryID}
	statement := `SELECT ` + issueReadColumns + ` FROM ` + issueFrom + ` WHERE issues.repository_id = $1`
	if query.State != "" {
		statement += ` AND issues.state = $2`
		args = append(args, string(query.State))
	}
	// The milestone filter, whose two shapes are genuinely different questions: an identifier narrows to
	// one milestone, and `NoMilestone` asks for the issues nobody has scheduled.
	switch {
	case query.MilestoneID != "":
		statement += fmt.Sprintf(` AND issues.milestone_id = $%d`, len(args)+1)
		args = append(args, query.MilestoneID)
	case query.NoMilestone:
		statement += ` AND issues.milestone_id IS NULL`
	}
	if query.Before != nil {
		// Strictly older, with the identifier breaking a tie on the same instant. The
		// row-comparison form is what lets the index satisfy the predicate.
		statement += fmt.Sprintf(` AND (issues.created_at, issues.id) < ($%d, $%d)`,
			len(args)+1, len(args)+2)
		args = append(args, query.Before.CreatedAt, query.Before.ID)
	}
	// Newest first, with the identifier as the tie-break so the order is total — the
	// data-model rule that "every paginated query has a deterministic total order".
	statement += fmt.Sprintf(` ORDER BY issues.created_at DESC, issues.id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return Page{}, fmt.Errorf("list the issues: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := Page{Issues: []Issue{}}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return Page{}, fmt.Errorf("read an issue: %w", err)
		}
		page.Issues = append(page.Issues, issue)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("list the issues: %w", err)
	}

	if len(page.Issues) > limit {
		page.HasMore = true
		page.Issues = page.Issues[:limit]
	}
	if page.HasMore && len(page.Issues) > 0 {
		next := SortKeyOf(page.Issues[len(page.Issues)-1])
		page.Next = &next
	}
	return page, nil
}

// Edit applies a title and body, guarded by the expected version.
//
// The version check is in the `WHERE` clause rather than in a read before it, so it cannot race
// the write it guards. A missing row and a moved version are reported differently, because the
// caller's next action differs: one is a conflict to resolve, the other means the issue is gone.
func (s *PostgresStore) Edit(ctx context.Context, input StoreEdit) (Issue, error) {
	var updated Issue
	err := s.change(ctx, audit.Event{}, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `
			UPDATE issues
			SET title = $3, body = $4, version = version + 1, updated_at = NOW()
			WHERE repository_id = $1 AND number = $2 AND version = $5
			RETURNING id`,
			input.RepositoryID, input.Number, input.Title, input.Body,
			input.ExpectedVersion).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			// Either the issue is gone or its version moved. The two are told apart by a read,
			// which only runs on the failure path.
			if _, getErr := s.Get(ctx, input.RepositoryID, input.Number); errors.Is(getErr, ErrNotFound) {
				return ErrNotFound
			}
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("edit the issue: %w", err)
		}

		updated, err = readIssue(ctx, tx, `WHERE issues.id = $1`, id)
		if err != nil {
			return fmt.Errorf("read the edited issue: %w", err)
		}
		// No record is written, because the catalogue defines no action for an edit of an
		// issue's text. `change` skips the write when the action is empty, so the absence is
		// explicit at both ends rather than implied by a missing call.
		return nil
	})
	if err != nil {
		return Issue{}, err
	}
	return updated, nil
}

// SetState applies a lifecycle transition.
//
// The expected state is in the `WHERE` clause, so a concurrent close and reopen cannot both
// win: the second sees a state it did not expect and is refused as a conflict. `closed_at` is
// set from the database's clock rather than the application's, per the typing rule.
func (s *PostgresStore) SetState(ctx context.Context, input StoreSetState) (Issue, error) {
	var updated Issue
	err := s.change(ctx, input.Event, func(tx *sql.Tx) error {
		// Closing stamps the close and its actor; reopening clears both, because the pair
		// constraint requires the two columns to move together with the state.
		//
		// **The two statements take different parameter counts, and that is the point.** A close
		// names the actor in `$4`; a reopen sets both columns to NULL and so has no `$4` at all.
		// An earlier version kept the placeholder in both and passed four arguments to a statement
		// expecting three, which PostgreSQL reported as "could not determine data type of
		// parameter $4" — caught by the round-trip test, which is the only place a reopen is
		// exercised against the real database.
		var id string
		var err error
		if input.State == StateClosed {
			err = tx.QueryRowContext(ctx, `
				UPDATE issues
				SET state = $3, closed_at = NOW(), closed_by = $4,
				    version = version + 1, updated_at = NOW()
				WHERE repository_id = $1 AND number = $2 AND state = $5
				RETURNING id`,
				input.RepositoryID, input.Number, string(input.State),
				nullableString(input.Event.ActorID), string(input.Expect)).Scan(&id)
		} else {
			err = tx.QueryRowContext(ctx, `
				UPDATE issues
				SET state = $3, closed_at = NULL, closed_by = NULL,
				    version = version + 1, updated_at = NOW()
				WHERE repository_id = $1 AND number = $2 AND state = $4
				RETURNING id`,
				input.RepositoryID, input.Number, string(input.State), string(input.Expect)).Scan(&id)
		}
		if errors.Is(err, sql.ErrNoRows) {
			// The issue is gone, or its state moved under the caller. Told apart by a read,
			// which runs only here.
			if _, getErr := s.Get(ctx, input.RepositoryID, input.Number); errors.Is(getErr, ErrNotFound) {
				return ErrNotFound
			}
			return ErrAlreadyInState
		}
		if err != nil {
			return fmt.Errorf("change the issue state: %w", err)
		}

		updated, err = readIssue(ctx, tx, `WHERE issues.id = $1`, id)
		if err != nil {
			return fmt.Errorf("read the updated issue: %w", err)
		}
		return audit.Record(ctx, tx, input.Event)
	})
	if err != nil {
		return Issue{}, err
	}
	return updated, nil
}

// Delete removes an issue, refusing one that is not there.
//
// The record is written in the same transaction as the delete, and the ledger has no foreign
// key to this table, so the record outlives the row — which is what makes a hard delete
// auditable.
func (s *PostgresStore) Delete(ctx context.Context, input StoreDelete) error {
	return s.change(ctx, input.Event, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM issues WHERE repository_id = $1 AND number = $2`,
			input.RepositoryID, input.Number)
		if err != nil {
			return fmt.Errorf("delete the issue: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete the issue: %w", err)
		}
		if affected == 0 {
			// Nothing was deleted, so the record must not be written either: a record of a
			// deletion that did not happen is worse than no record.
			return ErrNotFound
		}
		return audit.Record(ctx, tx, input.Event)
	})
}

// Count reports how many issues a repository has in a state.
func (s *PostgresStore) Count(ctx context.Context, repositoryID string, state State) (int, error) {
	statement := `SELECT count(*) FROM issues WHERE repository_id = $1`
	args := []any{repositoryID}
	if state != "" {
		statement += ` AND state = $2`
		args = append(args, string(state))
	}

	var count int
	if err := s.db.QueryRowContext(ctx, statement, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count the issues: %w", err)
	}
	return count, nil
}

// change runs a mutation in a transaction, validating its record first.
//
// **The record is written by the mutation, not here**, because it must be written inside the
// same transaction and only after the row it describes has actually changed — `Delete` has to
// learn that nothing matched before it decides whether to record anything. Writing it here
// instead would either duplicate the closure's call or record a change that did not happen.
//
// **An empty action means "this change writes no record", and that is a first-class outcome
// rather than a mistake.** An edit of an issue's text has no catalogue entry, so `Edit` passes
// an empty event, `Validate` — which refuses an empty action — is not reached, and the closure
// records nothing. Making the case explicit here means the absence is decided in one place
// rather than implied by a caller forgetting to pass something.
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
	// A rollback after a successful commit is a no-op, so this is safe on every path and is what
	// makes a failure mid-way leave nothing behind.
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

// readIssue reads one issue through the shared projection.
func readIssue(ctx context.Context, q queryer, where string, args ...any) (Issue, error) {
	row := q.QueryRowContext(ctx, `SELECT `+issueReadColumns+` FROM `+issueFrom+` `+where, args...)
	issue, err := scanIssue(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, ErrNotFound
	}
	if err != nil {
		return Issue{}, fmt.Errorf("read the issue: %w", err)
	}
	return issue, nil
}

// rowScanner is what a single-row read needs, so `*sql.Row` and `*sql.Rows` share one scan.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanIssue reads one row of the shared projection.
//
// The nullable columns are scanned through `sql.Null*` rather than into their zero values,
// because an empty string and an absent author are different facts: a deleted author must not
// read as an author whose identifier is the empty string, and a joined username is null exactly
// when there is no author to name.
func scanIssue(row rowScanner) (Issue, error) {
	var issue Issue
	var state string
	var authorID, authorName, closedBy, closedByName sql.NullString
	var closedAt sql.NullTime

	if err := row.Scan(&issue.ID, &issue.RepositoryID, &issue.Number, &issue.Title, &issue.Body,
		&authorID, &authorName, &state, &closedAt, &closedBy, &closedByName,
		&issue.CreatedAt, &issue.UpdatedAt, &issue.Version); err != nil {
		return Issue{}, err
	}

	issue.State = State(state)
	issue.AuthorID = authorID.String
	issue.AuthorName = authorName.String
	issue.ClosedBy = closedBy.String
	issue.ClosedByName = closedByName.String
	if closedAt.Valid {
		at := closedAt.Time
		issue.ClosedAt = &at
	}
	return issue, nil
}

// nullableString renders an empty string as SQL NULL.
//
// It is used for `author_id` and `closed_by`, which are null when there is nobody to name:
// inserting an empty string would satisfy neither the foreign key nor the pair constraint, and
// would turn "nobody" into "an account whose identifier is empty".
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
