package milestones

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/jackc/pgx/v5/pgconn"
)

// The PostgreSQL implementation.
//
// # The number allocation
//
// `Create` allocates the number with the same statement the issue numbering uses, against the
// milestone's own counter:
//
//	UPDATE repositories SET next_milestone_number = next_milestone_number + 1
//	WHERE id = $1 RETURNING next_milestone_number - 1
//
// So the same three properties follow, and all three are required by `urls-and-identifiers.md` §6.5 and
// L380:
//
//   - **Two concurrent creates never share a number** — the `UPDATE` takes the repository row's lock.
//   - **A failed transaction leaves no gap** — the increment is part of the transaction, so a rollback
//     undoes it. A `SEQUENCE` cannot do this, because `nextval` is not transactional.
//   - **The number is never reused** — because it is a counter, not a rank. This is the property the
//     comment numbering deliberately does *not* have, and the reason the mechanisms differ.
//
// # The closed-milestone guard is in the writing statement
//
// "Closed milestones reject new attachments" is enforced by the `UPDATE ... FROM milestones` predicate
// rather than only by the service's check before it. A close that raced an attachment would otherwise
// slip through between the check and the write; putting the state in the `WHERE` clause is what makes the
// two operations mutually exclusive, and it is the same reasoning the issue store's version guard uses.
//
// The statement also requires the issue and the milestone to be in the **same repository**, so an
// identifier from one repository cannot be attached to an issue in another even if a caller supplies it.

// milestoneColumns is the projection every read shares, so a scan can never drift from the query that
// feeds it.
//
// `title_folded` is deliberately **absent**: it is a lookup key, never a value a caller needs, and
// leaving it out keeps the scan and the projection in step.
const milestoneColumns = `id, repository_id, number, title, description, due_on, state, closed_at,
	created_at, updated_at`

// PostgresStore is the database-backed store.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a store over a database handle.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Create allocates a number and stores the milestone.
func (s *PostgresStore) Create(ctx context.Context, input StoreCreate) (Milestone, error) {
	var created Milestone
	err := s.change(ctx, audit.Event{}, func(tx *sql.Tx) error {
		var number int64
		err := tx.QueryRowContext(ctx, `
			UPDATE repositories SET next_milestone_number = next_milestone_number + 1
			WHERE id = $1
			RETURNING next_milestone_number - 1`, input.RepositoryID).Scan(&number)
		if errors.Is(err, sql.ErrNoRows) {
			// The repository was deleted between the service's access check and this transaction.
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("allocate the milestone number: %w", err)
		}

		row := tx.QueryRowContext(ctx, `
			INSERT INTO milestones (id, repository_id, number, title, title_folded, description, due_on)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+milestoneColumns,
			input.ID, input.RepositoryID, number, input.Title, input.TitleFolded,
			input.Description, nullTime(input.DueOn))

		milestone, err := scanMilestone(row)
		if err != nil {
			return translateMilestoneError(err)
		}
		created = milestone
		return nil
	})
	if err != nil {
		return Milestone{}, err
	}
	return created, nil
}

// GetByNumber returns one milestone by its repository-scoped number.
func (s *PostgresStore) GetByNumber(ctx context.Context, repositoryID string, number int64) (Milestone, error) {
	return readMilestone(ctx, s.db,
		`WHERE repository_id = $1 AND number = $2`, repositoryID, number)
}

// List returns a repository's milestones, newest first.
//
// Newest first rather than by number, because a milestone list is read as a plan: the one just created is
// the one being worked on. The order is `(created_at DESC, id DESC)`, which is total — the identifier
// breaks a tie on the same instant — so a page cannot repeat or drop a row.
func (s *PostgresStore) List(ctx context.Context, repositoryID string, state State) ([]Milestone, error) {
	statement := `SELECT ` + milestoneColumns + ` FROM milestones WHERE repository_id = $1`
	args := []any{repositoryID}
	if state != "" {
		statement += ` AND state = $2`
		args = append(args, string(state))
	}
	statement += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list the milestones: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMilestones(rows)
}

// Update applies a title, description, and due date, and writes **no ledger record**.
//
// `DueOnSet` distinguishes "leave the due date alone" from "clear it": a nil pointer alone cannot express
// both, so the statement uses a `CASE` when the caller did not name the field.
func (s *PostgresStore) Update(ctx context.Context, input StoreUpdate) (Milestone, error) {
	var updated Milestone
	err := s.change(ctx, audit.Event{}, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			UPDATE milestones
			SET title = $3, title_folded = $4, description = $5,
			    due_on = CASE WHEN $6 THEN $7::timestamptz ELSE due_on END,
			    updated_at = NOW()
			WHERE repository_id = $1 AND number = $2
			RETURNING `+milestoneColumns,
			input.RepositoryID, input.Number, input.Title, input.TitleFolded,
			input.Description, input.DueOnSet, nullTime(input.DueOn))

		milestone, err := scanMilestone(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return translateMilestoneError(err)
		}
		updated = milestone
		return nil
	})
	if err != nil {
		return Milestone{}, err
	}
	return updated, nil
}

// SetState applies a close or a reopen.
//
// The expected state is in the `WHERE` clause, so a concurrent close and reopen cannot both win. The
// record is written only when one was given: a reopen passes an empty event because the catalogue defines
// `milestone.closed` and no `milestone.opened`, so the absence is decided by the caller rather than
// implicit here.
func (s *PostgresStore) SetState(ctx context.Context, input StoreSetState) (Milestone, error) {
	recording := input.Event.Action != ""
	if recording {
		if err := input.Event.Validate(); err != nil {
			return Milestone{}, err
		}
	}

	var updated Milestone
	err := s.change(ctx, input.Event, func(tx *sql.Tx) error {
		var row *sql.Row
		if input.State == StateClosed {
			row = tx.QueryRowContext(ctx, `
				UPDATE milestones
				SET state = $3, closed_at = NOW(), updated_at = NOW()
				WHERE repository_id = $1 AND number = $2 AND state = $4
				RETURNING `+milestoneColumns,
				input.RepositoryID, input.Number, string(input.State), string(input.Expect))
		} else {
			row = tx.QueryRowContext(ctx, `
				UPDATE milestones
				SET state = $3, closed_at = NULL, updated_at = NOW()
				WHERE repository_id = $1 AND number = $2 AND state = $4
				RETURNING `+milestoneColumns,
				input.RepositoryID, input.Number, string(input.State), string(input.Expect))
		}

		milestone, err := scanMilestone(row)
		if errors.Is(err, sql.ErrNoRows) {
			// The milestone is gone, or its state moved under the caller. Told apart by a read, which runs
			// only here.
			if _, getErr := s.GetByNumber(ctx, input.RepositoryID, input.Number); errors.Is(getErr, ErrNotFound) {
				return ErrNotFound
			}
			return ErrAlreadyInState
		}
		if err != nil {
			return fmt.Errorf("change the milestone state: %w", err)
		}
		updated = milestone

		if !recording {
			return nil
		}
		// The record's scope was set by the service, which read the milestone before building it — see
		// `setState`. Filling it here would be too late: `change` validates the event before the
		// transaction opens.
		return audit.Record(ctx, tx, input.Event)
	})
	if err != nil {
		return Milestone{}, err
	}
	return updated, nil
}

// Delete removes a milestone, detaching its issues rather than deleting them.
//
// **The detachment is the foreign key's work, not this function's**: `issues.milestone_id` carries
// `ON DELETE SET NULL`, so the issues survive with no milestone. A hand-written `UPDATE issues` here would
// be a second statement a future code path could forget, and it would not protect the issues either — the
// constraint does that.
func (s *PostgresStore) Delete(ctx context.Context, input StoreDelete) error {
	return s.change(ctx, input.Event, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM milestones WHERE repository_id = $1 AND number = $2`,
			input.RepositoryID, input.Number)
		if err != nil {
			return fmt.Errorf("delete the milestone: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete the milestone: %w", err)
		}
		if affected == 0 {
			// Nothing was deleted, so the record must not be written either.
			return ErrNotFound
		}
		return audit.Record(ctx, tx, input.Event)
	})
}

// Progress returns how many issues carry each of a repository's milestones.
//
// **It is computed on every read**, which is what makes "progress counts stay correct after an issue is
// closed, reopened, or relabeled" true by construction: a derived count cannot go stale, and relabeling an
// issue cannot change it because labels are a different table.
//
// The join is a left join so a milestone with no issues appears with zero counts, which a milestone list
// needs — an inner join would silently omit an unused milestone.
func (s *PostgresStore) Progress(ctx context.Context, repositoryID string) (map[string]Progress, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT milestones.id,
		       count(issues.id) FILTER (WHERE issues.id IS NOT NULL),
		       count(issues.id) FILTER (WHERE issues.state = 'closed')
		FROM milestones
		LEFT JOIN issues ON issues.milestone_id = milestones.id
		WHERE milestones.repository_id = $1
		GROUP BY milestones.id`, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("count the milestones' issues: %w", err)
	}
	defer func() { _ = rows.Close() }()

	progress := map[string]Progress{}
	for rows.Next() {
		var id string
		var counts Progress
		if err := rows.Scan(&id, &counts.Total, &counts.Closed); err != nil {
			return nil, fmt.Errorf("read a milestone count: %w", err)
		}
		progress[id] = counts
	}
	return progress, rows.Err()
}

// Attach sets an issue's milestone.
//
// **The milestone must be open and in the same repository as the issue**, and both facts are in the
// writing statement rather than only in a check before it — so a close racing this attachment cannot slip
// between the two, and an identifier from another repository cannot be attached.
func (s *PostgresStore) Attach(ctx context.Context, input StoreAttach) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE issues
		SET milestone_id = m.id, updated_at = NOW()
		FROM milestones AS m
		WHERE issues.id = $1
		  AND m.id = $2
		  AND m.state = 'open'
		  AND issues.repository_id = m.repository_id`,
		input.IssueID, input.MilestoneID)
	if err != nil {
		return fmt.Errorf("attach the issue: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("attach the issue: %w", err)
	}
	if affected == 0 {
		// Nothing was attached, and the reason matters to the caller: the milestone may have closed since
		// the service checked, or the two may not be in one repository. A read explains it.
		return s.explainAttach(ctx, input)
	}
	return nil
}

// explainAttach reports why an attachment changed no row.
func (s *PostgresStore) explainAttach(ctx context.Context, input StoreAttach) error {
	var state string
	err := s.db.QueryRowContext(ctx, `
		SELECT state FROM milestones WHERE id = $1`, input.MilestoneID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read the milestone state: %w", err)
	}
	if State(state) != StateOpen {
		return ErrClosed
	}
	// The milestone is open and the statement still matched nothing, so the issue is gone or the two are
	// in different repositories. Both are a not-found to the caller: neither is reachable from where they
	// asked.
	return ErrNotFound
}

// Detach clears an issue's milestone, reporting whether it had one.
func (s *PostgresStore) Detach(ctx context.Context, issueID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE issues SET milestone_id = NULL, updated_at = NOW()
		WHERE id = $1 AND milestone_id IS NOT NULL`, issueID)
	if err != nil {
		return false, fmt.Errorf("detach the issue: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("detach the issue: %w", err)
	}
	return affected > 0, nil
}

// MilestoneOf returns the milestone number an issue carries, or zero when it carries none.
func (s *PostgresStore) MilestoneOf(ctx context.Context, issueID string) (int64, error) {
	var number sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT milestones.number
		FROM issues
		LEFT JOIN milestones ON milestones.id = issues.milestone_id
		WHERE issues.id = $1`, issueID).Scan(&number)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read the issue's milestone: %w", err)
	}
	return number.Int64, nil
}

// change runs a mutation in a transaction, validating its record first.
//
// The record is written by the mutation, not here, because it must be written inside the same transaction
// and only after the row it describes has actually changed. An empty action means "this change writes no
// record", which is what a create, an update, and a reopen pass.
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

// readMilestone reads one milestone through the shared projection.
func readMilestone(ctx context.Context, q queryer, where string, args ...any) (Milestone, error) {
	row := q.QueryRowContext(ctx, `SELECT `+milestoneColumns+` FROM milestones `+where, args...)
	milestone, err := scanMilestone(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Milestone{}, ErrNotFound
	}
	if err != nil {
		return Milestone{}, fmt.Errorf("read the milestone: %w", err)
	}
	return milestone, nil
}

// rowScanner is what a single-row read needs, so `*sql.Row` and `*sql.Rows` share one scan.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanMilestone reads one row of the shared projection.
//
// The nullable columns are scanned through `sql.Null*` rather than into their zero values, because an
// absent due date and an absent close are different facts from a zero instant — and reading a zero instant
// as "no date" would make the year 1 a legitimate due date.
func scanMilestone(row rowScanner) (Milestone, error) {
	var milestone Milestone
	var state string
	var dueOn, closedAt sql.NullTime

	if err := row.Scan(&milestone.ID, &milestone.RepositoryID, &milestone.Number, &milestone.Title,
		&milestone.Description, &dueOn, &state, &closedAt,
		&milestone.CreatedAt, &milestone.UpdatedAt); err != nil {
		return Milestone{}, err
	}

	milestone.State = State(state)
	if dueOn.Valid {
		// **Normalized to UTC on read**, because the field documents a date at midnight UTC and the driver
		// returns the instant in the session's location. Without this, `Hour()` reported 8 for a value that
		// is midnight UTC — the instant was right and its presentation was not, which is the kind of
		// difference that makes a comparison or a format depend on the server's timezone.
		at := dueOn.Time.UTC()
		milestone.DueOn = &at
	}
	if closedAt.Valid {
		at := closedAt.Time.UTC()
		milestone.ClosedAt = &at
	}
	return milestone, nil
}

// scanMilestones reads a result set of the shared projection.
func scanMilestones(rows *sql.Rows) ([]Milestone, error) {
	found := []Milestone{}
	for rows.Next() {
		milestone, err := scanMilestone(rows)
		if err != nil {
			return nil, fmt.Errorf("read a milestone: %w", err)
		}
		found = append(found, milestone)
	}
	return found, rows.Err()
}

// nullTime renders an absent instant as SQL NULL, so an absent due date is null rather than a zero
// instant.
func nullTime(at *time.Time) any {
	if at == nil {
		return nil
	}
	return *at
}

// translateMilestoneError turns a constraint violation into this package's error.
//
// The unique constraint on the folded title is the one a caller can cause. A title-length or state
// violation is unreachable from the service, which validates first, so an error here is a bug rather than a
// refusal and is wrapped rather than translated.
func translateMilestoneError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" && pgErr.ConstraintName == "milestones_repository_id_title_folded_key" {
			return ErrTitleTaken
		}
	}
	return err
}
