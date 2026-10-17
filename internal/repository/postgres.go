package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/jackc/pgx/v5/pgconn"
)

// schemaCheckSQL reports whether the repository columns exist in the schema the
// connection is using.
//
// It checks a column rather than the migration ledger, because a ledger row
// written by hand would otherwise claim a schema that is not there.
const schemaCheckSQL = `
	SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'repositories'
		  AND column_name = 'storage_id'
	)`

// auditSchemaCheckSQL reports whether the ledger exists, which phase 1 needs
// because every change writes a record in the same transaction.
const auditSchemaCheckSQL = `
	SELECT to_regclass(current_schema() || '.audit_events') IS NOT NULL`

// repositoryColumns is the projection every repository read shares.
//
// The folded name is deliberately absent: it is a lookup key, never a value a
// caller needs, and leaving it out keeps the scan and the projection in step.
const repositoryColumns = `id, owner_id, name, description, visibility,
	COALESCE(default_branch, ''), storage_id, archived_at IS NOT NULL, created_at, updated_at`

// PostgresStore persists repositories in PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a store backed by an open pool.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Create inserts a repository and its creation record in one transaction.
func (s *PostgresStore) Create(ctx context.Context, newRepository New, event audit.Event) (Repository, error) {
	return s.change(ctx, event, "create the repository", func(tx *sql.Tx) (Repository, error) {
		found, err := scanRepository(tx.QueryRowContext(ctx, `
			INSERT INTO repositories (id, owner_id, name, name_folded, description, visibility, storage_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+repositoryColumns,
			newRepository.ID, newRepository.OwnerID, newRepository.Name, newRepository.NameFolded,
			newRepository.Description, string(newRepository.Visibility), newRepository.StorageID,
		))
		if err != nil {
			return Repository{}, mapRepositoryError(err, "create the repository")
		}
		return found, nil
	})
}

// FindByID returns one repository.
func (s *PostgresStore) FindByID(ctx context.Context, id string) (Repository, error) {
	if err := s.requireSchema(ctx); err != nil {
		return Repository{}, err
	}
	found, err := scanRepository(s.db.QueryRowContext(ctx,
		"SELECT "+repositoryColumns+" FROM repositories WHERE id = $1", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, ErrNotFound
	}
	if err != nil {
		return Repository{}, fmt.Errorf("find the repository: %w", err)
	}
	return found, nil
}

// FindByOwnerAndFoldedName resolves a repository from an owner and a folded name.
func (s *PostgresStore) FindByOwnerAndFoldedName(ctx context.Context, ownerID, nameFolded string) (Repository, error) {
	if err := s.requireSchema(ctx); err != nil {
		return Repository{}, err
	}
	found, err := scanRepository(s.db.QueryRowContext(ctx,
		"SELECT "+repositoryColumns+" FROM repositories WHERE owner_id = $1 AND name_folded = $2",
		ownerID, nameFolded))
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, ErrNotFound
	}
	if err != nil {
		return Repository{}, fmt.Errorf("find the repository: %w", err)
	}
	return found, nil
}

// RoleFor returns the direct grant a user holds on a repository.
func (s *PostgresStore) RoleFor(ctx context.Context, repoID, userID string) (Level, bool, error) {
	if err := s.requireSchema(ctx); err != nil {
		return LevelNone, false, err
	}
	var role string
	err := s.db.QueryRowContext(ctx,
		"SELECT role FROM repository_collaborators WHERE repository_id = $1 AND user_id = $2",
		repoID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return LevelNone, false, nil
	}
	if err != nil {
		return LevelNone, false, fmt.Errorf("read the collaborator grant: %w", err)
	}
	level, err := parseRole(role)
	if err != nil {
		return LevelNone, false, err
	}
	return level, true, nil
}

// Rename changes the name, leaving owner_id and storage_id untouched.
func (s *PostgresStore) Rename(ctx context.Context, repoID, name, nameFolded string, event audit.Event) (Repository, error) {
	return s.change(ctx, event, "rename the repository", func(tx *sql.Tx) (Repository, error) {
		found, err := scanRepository(tx.QueryRowContext(ctx, `
			UPDATE repositories
			SET name = $2, name_folded = $3, updated_at = NOW()
			WHERE id = $1
			RETURNING `+repositoryColumns, repoID, name, nameFolded))
		if errors.Is(err, sql.ErrNoRows) {
			return Repository{}, ErrNotFound
		}
		if err != nil {
			return Repository{}, mapRepositoryError(err, "rename the repository")
		}
		return found, nil
	})
}

// SetVisibility changes the visibility of a repository.
func (s *PostgresStore) SetVisibility(ctx context.Context, repoID string, visibility Visibility, event audit.Event) (Repository, error) {
	return s.change(ctx, event, "change the visibility", func(tx *sql.Tx) (Repository, error) {
		found, err := scanRepository(tx.QueryRowContext(ctx, `
			UPDATE repositories
			SET visibility = $2, updated_at = NOW()
			WHERE id = $1
			RETURNING `+repositoryColumns, repoID, string(visibility)))
		if errors.Is(err, sql.ErrNoRows) {
			return Repository{}, ErrNotFound
		}
		if err != nil {
			return Repository{}, fmt.Errorf("change the visibility: %w", err)
		}
		return found, nil
	})
}

// SetDefaultBranch changes the default branch of a repository.
func (s *PostgresStore) SetDefaultBranch(ctx context.Context, repoID, branch string, event audit.Event) (Repository, error) {
	return s.change(ctx, event, "change the default branch", func(tx *sql.Tx) (Repository, error) {
		found, err := scanRepository(tx.QueryRowContext(ctx, `
			UPDATE repositories
			SET default_branch = $2, updated_at = NOW()
			WHERE id = $1
			RETURNING `+repositoryColumns, repoID, branch))
		if errors.Is(err, sql.ErrNoRows) {
			return Repository{}, ErrNotFound
		}
		if err != nil {
			return Repository{}, fmt.Errorf("change the default branch: %w", err)
		}
		return found, nil
	})
}

// SetArchived sets or clears the archive flag.
func (s *PostgresStore) SetArchived(ctx context.Context, repoID string, archived bool, event audit.Event) (Repository, error) {
	return s.change(ctx, event, "change the archive state", func(tx *sql.Tx) (Repository, error) {
		found, err := scanRepository(tx.QueryRowContext(ctx, `
			UPDATE repositories
			SET archived_at = CASE WHEN $2 THEN NOW() ELSE NULL END, updated_at = NOW()
			WHERE id = $1
			RETURNING `+repositoryColumns, repoID, archived))
		if errors.Is(err, sql.ErrNoRows) {
			return Repository{}, ErrNotFound
		}
		if err != nil {
			return Repository{}, fmt.Errorf("change the archive state: %w", err)
		}
		return found, nil
	})
}

// ListVisible returns the repositories the viewer may read, filtered before the
// page is taken.
//
// The visibility predicate is part of the WHERE clause, so LIMIT and OFFSET
// apply to rows the viewer is allowed to see. Filtering in Go after the fact
// would return short pages and, worse, would make a hidden repository's position
// observable through the page boundaries.
//
// The parameter list:
//
//	$1 owner filter, NULL for every owner
//	$2 the viewer's account, NULL when anonymous
//	$3 whether the viewer is an instance administrator
//	$4 limit, $5 offset
func (s *PostgresStore) ListVisible(ctx context.Context, query ListQuery) ([]Repository, error) {
	if err := s.requireSchema(ctx); err != nil {
		return nil, err
	}

	var ownerID, viewerID any
	if query.OwnerID != "" {
		ownerID = query.OwnerID
	}
	if !query.Viewer.IsAnonymous() {
		viewerID = query.Viewer.UserID
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+repositoryColumns+`
		FROM repositories
		WHERE ($1::uuid IS NULL OR owner_id = $1::uuid)
		  AND (
		        visibility = 'public'
		     OR ($2::uuid IS NOT NULL AND (
		            owner_id = $2::uuid
		            OR EXISTS (
		                SELECT 1 FROM repository_collaborators c
		                WHERE c.repository_id = repositories.id AND c.user_id = $2::uuid
		            )
		        ))
		     OR $3
		  )
		ORDER BY updated_at DESC, id DESC
		LIMIT $4 OFFSET $5`,
		ownerID, viewerID, query.Viewer.IsInstanceAdmin, query.Limit, query.Offset)
	if err != nil {
		return nil, fmt.Errorf("list the repositories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var found []Repository
	for rows.Next() {
		item, err := scanRepository(rows)
		if err != nil {
			return nil, fmt.Errorf("read a repository: %w", err)
		}
		found = append(found, item)
	}
	return found, rows.Err()
}

// SetCollaborator adds or updates a direct grant and records it.
//
// The insert is told apart from the update by `xmax`, which PostgreSQL sets on a
// row that the current statement inserted rather than updated. Doing it that way
// keeps the read and the write in one statement, so the record cannot describe a
// state the row was never in.
func (s *PostgresStore) SetCollaborator(ctx context.Context, repoID, userID string, role Level, events GrantEvents) (bool, error) {
	if err := s.requireSchema(ctx); err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin the grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var inserted bool
	err = tx.QueryRowContext(ctx, `
		INSERT INTO repository_collaborators (repository_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (repository_id, user_id)
		DO UPDATE SET role = EXCLUDED.role, updated_at = NOW()
		RETURNING (xmax = 0)`,
		repoID, userID, roleText(role)).Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("grant repository access: %w", err)
	}

	// The record that matches what happened is the one written, and it is written
	// in this transaction, so a failed record fails the grant.
	event := events.Changed
	if inserted {
		event = events.Added
	}
	if err := audit.Record(ctx, tx, event); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit the grant: %w", err)
	}
	return inserted, nil
}

// RemoveCollaborator revokes a direct grant and records the revocation.
func (s *PostgresStore) RemoveCollaborator(ctx context.Context, repoID, userID string, event audit.Event) error {
	if err := s.requireSchema(ctx); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the revocation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		"DELETE FROM repository_collaborators WHERE repository_id = $1 AND user_id = $2", repoID, userID)
	if err != nil {
		return fmt.Errorf("revoke repository access: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke repository access: %w", err)
	}
	if affected == 0 {
		// Nothing changed, so nothing is recorded: a record claiming a revocation
		// that did not happen would be worse than no record.
		return ErrNotFound
	}

	if err := audit.Record(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the revocation: %w", err)
	}
	return nil
}

// Collaborators lists a repository's direct grants.
func (s *PostgresStore) Collaborators(ctx context.Context, repoID string) ([]Collaborator, error) {
	if err := s.requireSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT user_id, role FROM repository_collaborators WHERE repository_id = $1 ORDER BY user_id", repoID)
	if err != nil {
		return nil, fmt.Errorf("list the collaborators: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var found []Collaborator
	for rows.Next() {
		var (
			userID string
			role   string
		)
		if err := rows.Scan(&userID, &role); err != nil {
			return nil, fmt.Errorf("read a collaborator: %w", err)
		}
		level, err := parseRole(role)
		if err != nil {
			return nil, err
		}
		found = append(found, Collaborator{UserID: userID, Role: level})
	}
	return found, rows.Err()
}

// change runs a mutation and its audit record in one transaction.
//
// Every mutation goes through it, which is what makes "a failed audit write
// fails the operation" true by construction rather than by each method
// remembering to do it.
func (s *PostgresStore) change(ctx context.Context, event audit.Event, action string, mutate func(*sql.Tx) (Repository, error)) (Repository, error) {
	if err := s.requireSchema(ctx); err != nil {
		return Repository{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Repository{}, fmt.Errorf("begin %s: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()

	found, err := mutate(tx)
	if err != nil {
		return Repository{}, err
	}
	if err := audit.Record(ctx, tx, event); err != nil {
		return Repository{}, err
	}
	if err := tx.Commit(); err != nil {
		return Repository{}, fmt.Errorf("%s: %w", action, err)
	}
	return found, nil
}

// requireSchema reports ErrSchemaNotMigrated before any query touches a table
// that may not exist, so an operator gets one actionable error rather than a
// driver error naming a missing relation.
func (s *PostgresStore) requireSchema(ctx context.Context) error {
	var ready bool
	if err := s.db.QueryRowContext(ctx, schemaCheckSQL).Scan(&ready); err != nil {
		return fmt.Errorf("check the repository schema: %w", err)
	}
	if !ready {
		return ErrSchemaNotMigrated
	}
	// The ledger arrives in the same migration as the archive flag, so a schema
	// with one and not the other is not a state the migrations can produce; the
	// check is here because a change without its record is the failure this
	// store exists to prevent.
	var ledger bool
	if err := s.db.QueryRowContext(ctx, auditSchemaCheckSQL).Scan(&ledger); err != nil {
		return fmt.Errorf("check the audit schema: %w", err)
	}
	if !ledger {
		return ErrSchemaNotMigrated
	}
	return nil
}

func scanRepository(row interface{ Scan(...any) error }) (Repository, error) {
	var (
		found      Repository
		visibility string
	)
	if err := row.Scan(
		&found.ID, &found.OwnerID, &found.Name, &found.Description, &visibility,
		&found.DefaultBranch, &found.StorageID, &found.Archived, &found.CreatedAt, &found.UpdatedAt,
	); err != nil {
		return Repository{}, err
	}
	found.Visibility = Visibility(visibility)
	return found, nil
}

// roleText maps a level to its stored role. Only read, write, and admin are
// storable: ownership is the repository's owner_id, not a grant.
func roleText(level Level) string {
	switch level {
	case LevelRead:
		return "read"
	case LevelWrite:
		return "write"
	case LevelAdmin:
		return "admin"
	default:
		// The service refuses anything else before it reaches here, so this is
		// a programming error rather than bad input.
		return "none"
	}
}

func parseRole(role string) (Level, error) {
	switch role {
	case "read":
		return LevelRead, nil
	case "write":
		return LevelWrite, nil
	case "admin":
		return LevelAdmin, nil
	default:
		return LevelNone, fmt.Errorf("stored collaborator role %q is not recognised", role)
	}
}

// mapRepositoryError turns the name-collision constraint into the sentinel a
// caller acts on, and leaves every other failure wrapped.
func mapRepositoryError(err error, action string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		strings.Contains(pgErr.ConstraintName, "name_folded") {
		return ErrNameTaken
	}
	return fmt.Errorf("%s: %w", action, err)
}
