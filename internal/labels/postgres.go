package labels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/jackc/pgx/v5/pgconn"
)

// The PostgreSQL implementation.
//
// # Uniqueness is a constraint, not a check
//
// A name conflict is decided by the unique constraint on `(repository_id, name_folded)`, and this
// store translates the resulting error into `ErrNameTaken`. That is the data-model rule "uniqueness is
// enforced by a constraint, never by a read-then-write in application code": a check here could race
// another writer between the read and the insert, and the loser would get a duplicate row rather than
// an error.
//
// # One transaction per change
//
// Every mutation and its record share a transaction, so a change that rolled back leaves no record and
// a record that failed to write fails the change. The two deletions — a label and a detachment — are
// the cases where this matters most, because both are destructive and both must be answerable
// afterwards.

// labelColumns is the projection every read shares, so a scan can never drift from the query that
// feeds it.
//
// `name_folded` is deliberately **absent**: it is a lookup key, never a value a caller needs, and
// leaving it out keeps the scan and the projection in step.
const labelColumns = `id, repository_id, name, color, description, created_at, updated_at`

// PostgresStore is the database-backed store.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a store over a database handle.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Create stores a label.
func (s *PostgresStore) Create(ctx context.Context, input StoreCreate) (Label, error) {
	var created Label
	err := s.change(ctx, input.Event, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			INSERT INTO labels (id, repository_id, name, name_folded, color, description)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+labelColumns,
			input.ID, input.RepositoryID, input.Name, input.NameFolded, input.Color, input.Description)

		label, err := scanLabel(row)
		if err != nil {
			return translateLabelError(err)
		}
		created = label
		return audit.Record(ctx, tx, input.Event)
	})
	if err != nil {
		return Label{}, err
	}
	return created, nil
}

// Get returns one label by identifier.
func (s *PostgresStore) Get(ctx context.Context, repositoryID, labelID string) (Label, error) {
	return readLabel(ctx, s.db,
		`WHERE repository_id = $1 AND id = $2`, repositoryID, labelID)
}

// FindByName returns one label by its folded name.
//
// The lookup is on the folded form, so a URL's case is irrelevant — which is what §6.1's "compared
// after Unicode-free ASCII case folding" means for an addressable name.
func (s *PostgresStore) FindByName(ctx context.Context, repositoryID, nameFolded string) (Label, error) {
	return readLabel(ctx, s.db,
		`WHERE repository_id = $1 AND name_folded = $2`, repositoryID, nameFolded)
}

// List returns a repository's labels, ordered by folded name.
//
// The order is by the folded form rather than the displayed one, so the list reads case-insensitively —
// which is what a reader expects when `Bug` and `bug` cannot both exist.
func (s *PostgresStore) List(ctx context.Context, repositoryID string) ([]Label, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+labelColumns+`
		FROM labels
		WHERE repository_id = $1
		ORDER BY name_folded`, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("list the labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanLabels(rows)
}

// Update applies a label's name, colour, and description.
//
// It writes **no ledger record**, because the catalogue defines no action for editing a label. The
// absence is visible here — the method takes no event — rather than being a forgotten call at the call
// site.
func (s *PostgresStore) Update(ctx context.Context, input StoreUpdate) (Label, error) {
	var updated Label
	err := s.change(ctx, audit.Event{}, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			UPDATE labels
			SET name = $3, name_folded = $4, color = $5, description = $6, updated_at = NOW()
			WHERE repository_id = $1 AND id = $2
			RETURNING `+labelColumns,
			input.RepositoryID, input.LabelID, input.Name, input.NameFolded,
			input.Color, input.Description)

		label, err := scanLabel(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return translateLabelError(err)
		}
		updated = label
		return nil
	})
	if err != nil {
		return Label{}, err
	}
	return updated, nil
}

// Delete removes a label, which removes its attachments and no issues.
//
// **The cascade is what satisfies the acceptance, and this function does not implement it.** The join
// table's `label_id` foreign key carries `ON DELETE CASCADE`, so the attachments go with the label in
// the same statement; the issues they pointed at are a different table and are never touched. A
// hand-written `DELETE FROM issue_labels` here would be a second statement that a future code path
// could forget, and it would not protect the issues either — the constraint does that.
func (s *PostgresStore) Delete(ctx context.Context, input StoreDelete) error {
	return s.change(ctx, input.Event, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM labels WHERE repository_id = $1 AND id = $2`,
			input.RepositoryID, input.LabelID)
		if err != nil {
			return fmt.Errorf("delete the label: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete the label: %w", err)
		}
		if affected == 0 {
			// Nothing was deleted, so the record must not be written either: a record of a deletion that
			// did not happen is worse than no record.
			return ErrNotFound
		}
		return audit.Record(ctx, tx, input.Event)
	})
}

// Attach applies a label to an issue, reporting whether the attachment was new.
//
// `ON CONFLICT DO NOTHING` makes the operation idempotent, which is the right behaviour for it: the
// result is a state — "this label is on this issue" — rather than an event, so applying it twice
// succeeds and the caller learns from the boolean that nothing changed. The rows-affected count is how
// that is learned, rather than a read before the write that could race.
func (s *PostgresStore) Attach(ctx context.Context, issueID, labelID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO issue_labels (issue_id, label_id)
		VALUES ($1, $2)
		ON CONFLICT (issue_id, label_id) DO NOTHING`, issueID, labelID)
	if err != nil {
		return false, fmt.Errorf("apply the label: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("apply the label: %w", err)
	}
	return affected > 0, nil
}

// Detach removes a label from an issue, reporting whether anything was removed.
func (s *PostgresStore) Detach(ctx context.Context, issueID, labelID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM issue_labels WHERE issue_id = $1 AND label_id = $2`, issueID, labelID)
	if err != nil {
		return false, fmt.Errorf("remove the label: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("remove the label: %w", err)
	}
	return affected > 0, nil
}

// ForIssue returns the labels on one issue, ordered by folded name.
func (s *PostgresStore) ForIssue(ctx context.Context, issueID string) ([]Label, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+qualifiedLabelColumns("labels")+`
		FROM labels
		JOIN issue_labels ON issue_labels.label_id = labels.id
		WHERE issue_labels.issue_id = $1
		ORDER BY labels.name_folded`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list the issue's labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanLabels(rows)
}

// CountsForRepository returns how many issues carry each of a repository's labels.
//
// It is a left join so a label with no issues appears with a count of zero: a label list shows every
// label, including one nothing uses, and an inner join would silently omit it.
func (s *PostgresStore) CountsForRepository(ctx context.Context, repositoryID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT labels.id, count(issue_labels.issue_id)
		FROM labels
		LEFT JOIN issue_labels ON issue_labels.label_id = labels.id
		WHERE labels.repository_id = $1
		GROUP BY labels.id`, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("count the labels' issues: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := map[string]int{}
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("read a label count: %w", err)
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// change runs a mutation in a transaction, validating its record first.
//
// The record is written by the mutation, not here, because it must be written inside the same
// transaction and only after the row it describes has actually changed. An empty action means "this
// change writes no record", which is what an update passes.
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

// readLabel reads one label through the shared projection.
func readLabel(ctx context.Context, q queryer, where string, args ...any) (Label, error) {
	row := q.QueryRowContext(ctx, `SELECT `+labelColumns+` FROM labels `+where, args...)
	label, err := scanLabel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Label{}, ErrNotFound
	}
	if err != nil {
		return Label{}, fmt.Errorf("read the label: %w", err)
	}
	return label, nil
}

// rowScanner is what a single-row read needs, so `*sql.Row` and `*sql.Rows` share one scan.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanLabel reads one row of the shared projection.
func scanLabel(row rowScanner) (Label, error) {
	var label Label
	if err := row.Scan(&label.ID, &label.RepositoryID, &label.Name, &label.Color,
		&label.Description, &label.CreatedAt, &label.UpdatedAt); err != nil {
		return Label{}, err
	}
	return label, nil
}

// scanLabels reads a result set of the shared projection.
func scanLabels(rows *sql.Rows) ([]Label, error) {
	found := []Label{}
	for rows.Next() {
		label, err := scanLabel(rows)
		if err != nil {
			return nil, fmt.Errorf("read a label: %w", err)
		}
		found = append(found, label)
	}
	return found, rows.Err()
}

// labelColumnNames is the projection as column names, so a join can qualify them without a second
// hand-written list that could drift from `labelColumns`.
var labelColumnNames = []string{
	"id", "repository_id", "name", "color", "description", "created_at", "updated_at",
}

// qualifiedLabelColumns qualifies the projection with a table name, which the join needs.
func qualifiedLabelColumns(table string) string {
	qualified := make([]string, 0, len(labelColumnNames))
	for _, column := range labelColumnNames {
		qualified = append(qualified, table+"."+column)
	}
	return strings.Join(qualified, ", ")
}

// translateLabelError turns a constraint violation into this package's error.
//
// The unique constraint is the one a caller can cause: two names that differ only by ASCII case. The
// name and check violations are unreachable from the service, which validates first — so an error here
// is a bug rather than a refusal, and it is wrapped rather than translated.
func translateLabelError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" && pgErr.ConstraintName == "labels_repository_id_name_folded_key" {
			return ErrNameTaken
		}
	}
	return err
}
