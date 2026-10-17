package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// bootstrapLockKey serializes concurrent bootstrap attempts with a
// transaction-scoped advisory lock. The value is arbitrary but must not be
// reused by another GitNest lock.
//
// docs/architecture/data-model.md names this pattern for a singleton operation:
// an advisory lock plus an existence check, rather than a database constraint
// that would also block the role from being granted later.
const bootstrapLockKey int64 = 0x6769746e65737401

// schemaCheckSQL reports whether the account columns exist in the schema the
// connection is using.
//
// It checks the column rather than the migration ledger, because a ledger row
// written by hand would otherwise claim a schema that is not there.
const schemaCheckSQL = `
	SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'users'
		  AND column_name = 'is_admin'
	)`

// PostgresStore persists accounts in PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore returns a store backed by an open pool.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// CreateInitialAdmin creates the instance's first administrator inside one
// transaction.
//
// The advisory lock plus the existence check mean a repeated or concurrent
// bootstrap fails with ErrAdminExists instead of creating a second
// administrator, and a failure leaves no account behind because the check, the
// insert, and the ledger of what happened share one transaction.
func (s *PostgresStore) CreateInitialAdmin(ctx context.Context, admin NewAdmin) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin the administrator bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", bootstrapLockKey); err != nil {
		return User{}, fmt.Errorf("lock the administrator bootstrap: %w", err)
	}

	migrated, err := schemaReady(ctx, tx)
	if err != nil {
		return User{}, err
	}
	if !migrated {
		return User{}, ErrSchemaNotMigrated
	}

	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM users WHERE is_admin)").Scan(&exists); err != nil {
		return User{}, fmt.Errorf("check for an existing administrator: %w", err)
	}
	if exists {
		return User{}, ErrAdminExists
	}

	user := User{ID: admin.ID, Username: admin.Username, Email: admin.Email, IsAdmin: true}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO users (id, username, username_folded, email, password_hash, is_admin)
		VALUES ($1, $2, $3, $4, $5, TRUE)
		RETURNING created_at`,
		admin.ID, admin.Username, admin.UsernameFolded, admin.Email, admin.PasswordHash,
	).Scan(&user.CreatedAt)
	if err != nil {
		return User{}, mapInsertError(err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit the administrator bootstrap: %w", err)
	}
	return user, nil
}

func schemaReady(ctx context.Context, tx *sql.Tx) (bool, error) {
	var ready bool
	if err := tx.QueryRowContext(ctx, schemaCheckSQL).Scan(&ready); err != nil {
		return false, fmt.Errorf("check the account schema: %w", err)
	}
	return ready, nil
}

// userColumns is the projection every account read shares, so a new column is
// added in one place.
const userColumns = "id, username, email, status, is_admin, created_at"

// scanUser reads one account row.
func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var (
		user   User
		status string
	)
	if err := row.Scan(&user.ID, &user.Username, &user.Email, &status, &user.IsAdmin, &user.CreatedAt); err != nil {
		return User{}, err
	}
	user.Status = Status(status)
	return user, nil
}

// FindUserForLogin returns the account and the stored password hash for a
// username or email address.
//
// The caller passes a folded login, so the comparison uses the folded column and
// the stored address, both of which are stored in their comparison form. An
// unknown account reports ErrInvalidCredentials rather than "not found", so the
// caller cannot distinguish it from a wrong password.
func (s *PostgresStore) FindUserForLogin(ctx context.Context, login string) (User, string, error) {
	var (
		user   User
		status string
		hash   string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT `+userColumns+`, password_hash
		FROM users
		WHERE username_folded = $1 OR email = $1`, login,
	).Scan(&user.ID, &user.Username, &user.Email, &status, &user.IsAdmin, &user.CreatedAt, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return User{}, "", fmt.Errorf("find the account for a sign-in: %w", err)
	}
	user.Status = Status(status)
	return user, hash, nil
}

// FindUserByID returns one account.
func (s *PostgresStore) FindUserByID(ctx context.Context, id string) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = $1", id))
	if errors.Is(err, sql.ErrNoRows) {
		// A caller that cannot be found is treated as unauthenticated rather
		// than as a distinguishable error.
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, fmt.Errorf("find the account: %w", err)
	}
	return user, nil
}

// FindUserByUsername returns one account by its folded username.
//
// The comparison uses the folded column, so the lookup agrees with the unique
// constraint rather than depending on the database collation.
func (s *PostgresStore) FindUserByUsername(ctx context.Context, usernameFolded string) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE username_folded = $1", usernameFolded))
	if errors.Is(err, sql.ErrNoRows) {
		// An unknown owner is reported like an unknown account, so a caller
		// cannot tell "no such owner" from "an account you may not see".
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, fmt.Errorf("find the account by username: %w", err)
	}
	return user, nil
}

// CreateSession stores a new session.
func (s *PostgresStore) CreateSession(ctx context.Context, session NewSession) (Session, error) {
	created := Session{ID: session.ID, UserID: session.UserID, ExpiresAt: session.ExpiresAt}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, csrf_token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`,
		session.ID, session.UserID, session.TokenHash, session.CSRFTokenHash, session.ExpiresAt,
	).Scan(&created.CreatedAt)
	if err != nil {
		return Session{}, fmt.Errorf("create the session: %w", err)
	}
	return created, nil
}

// FindSessionByToken resolves a session token digest to its account, its session
// row, and its CSRF token digest.
//
// Unknown, expired, and revoked sessions all report ErrInvalidToken, and an
// account disabled after sign-in is refused here too, so disabling an account
// takes effect on the next request rather than only at the next sign-in.
func (s *PostgresStore) FindSessionByToken(ctx context.Context, tokenHash []byte) (User, Session, []byte, error) {
	var (
		user      User
		session   Session
		status    string
		csrfHash  []byte
		createdAt time.Time
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.email, u.status, u.is_admin, u.created_at,
		       s.id, s.user_id, s.created_at, s.expires_at, s.csrf_token_hash
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > NOW()
		  AND u.status = 'active'`, tokenHash,
	).Scan(
		&user.ID, &user.Username, &user.Email, &status, &user.IsAdmin, &user.CreatedAt,
		&session.ID, &session.UserID, &createdAt, &session.ExpiresAt, &csrfHash,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Session{}, nil, ErrInvalidToken
	}
	if err != nil {
		return User{}, Session{}, nil, fmt.Errorf("find the session: %w", err)
	}
	user.Status = Status(status)
	session.CreatedAt = createdAt
	return user, session, csrfHash, nil
}

// RevokeSessionByToken revokes one session, reporting ErrInvalidToken when no
// live session matches.
func (s *PostgresStore) RevokeSessionByToken(ctx context.Context, tokenHash []byte) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = NOW()
		WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash)
	if err != nil {
		return fmt.Errorf("revoke the session: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke the session: %w", err)
	}
	if affected == 0 {
		return ErrInvalidToken
	}
	return nil
}

// RevokeSessionsForUser revokes every live session of one account.
func (s *PostgresStore) RevokeSessionsForUser(ctx context.Context, userID string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = NOW()
		WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return fmt.Errorf("revoke the account's sessions: %w", err)
	}
	return nil
}

// UpdateSessionCSRFTokenHash replaces the CSRF token digest of one live session.
//
// Only the digest is written, never the token: the row cannot be used to recover
// a token, and a database leak yields nothing to present to the API
// (docs/architecture/secrets.md).
//
// The update is scoped to an unrevoked session, so a rotation cannot resurrect a
// session that was revoked between the resolution and the write — which is the
// same race the revocation rule guards, and the reason this is one statement
// rather than a read and a write.
func (s *PostgresStore) UpdateSessionCSRFTokenHash(ctx context.Context, sessionID string, csrfTokenHash []byte) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET csrf_token_hash = $2
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > NOW()`, sessionID, csrfTokenHash)
	if err != nil {
		return fmt.Errorf("rotate the session's CSRF token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rotate the session's CSRF token: %w", err)
	}
	if affected == 0 {
		// The session stopped being live between the resolution and this write,
		// so the caller must not hand out a token for it.
		return ErrInvalidToken
	}
	return nil
}

// CreateInvitation stores a new single-use invitation.
func (s *PostgresStore) CreateInvitation(ctx context.Context, invitation NewInvitation) (Invitation, error) {
	created := Invitation{
		ID:        invitation.ID,
		Email:     invitation.Email,
		CreatedBy: invitation.CreatedBy,
		ExpiresAt: invitation.ExpiresAt,
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO invitations (id, email, token_hash, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`,
		invitation.ID, invitation.Email, invitation.TokenHash, invitation.CreatedBy, invitation.ExpiresAt,
	).Scan(&created.CreatedAt)
	if err != nil {
		return Invitation{}, fmt.Errorf("create the invitation: %w", err)
	}
	return created, nil
}

// FindInvitationByToken resolves an invitation by token digest.
//
// Unknown, expired, accepted, and revoked invitations all report
// ErrInvalidToken, so a caller cannot learn which of those it was.
func (s *PostgresStore) FindInvitationByToken(ctx context.Context, tokenHash []byte) (Invitation, error) {
	var (
		invitation Invitation
		createdAt  time.Time
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, email, created_by, created_at, expires_at
		FROM invitations
		WHERE token_hash = $1
		  AND revoked_at IS NULL
		  AND accepted_at IS NULL
		  AND expires_at > NOW()`, tokenHash,
	).Scan(&invitation.ID, &invitation.Email, &invitation.CreatedBy, &createdAt, &invitation.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvalidToken
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("find the invitation: %w", err)
	}
	invitation.CreatedAt = createdAt
	return invitation, nil
}

// AcceptInvitation creates the account and consumes the invitation in one
// transaction.
//
// The account is inserted before the invitation is claimed, because the claim
// writes the accepting account's identifier at the same time as the acceptance
// instant — the table requires those two to agree. The claim itself is a
// conditional UPDATE, which is the serialization point: a replayed or concurrent
// acceptance finds no unaccepted row, reports ErrInvalidToken, and its
// transaction rolls the account back with it.
func (s *PostgresStore) AcceptInvitation(ctx context.Context, invitationID string, account NewAccount) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin the invitation acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The address comes from the invitation row, never from the request, so an
	// invitee cannot redirect the invitation to another address.
	var email string
	err = tx.QueryRowContext(ctx, `
		SELECT email FROM invitations
		WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > NOW()`,
		invitationID).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrInvalidToken
	}
	if err != nil {
		return User{}, fmt.Errorf("read the invitation: %w", err)
	}

	user := User{ID: account.ID, Username: account.Username, Email: email, Status: StatusActive}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO users (id, username, username_folded, email, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING is_admin, created_at`,
		account.ID, account.Username, account.UsernameFolded, email, account.PasswordHash,
	).Scan(&user.IsAdmin, &user.CreatedAt)
	if err != nil {
		return User{}, mapInsertError(err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE invitations SET accepted_at = NOW(), accepted_by = $2
		WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > NOW()`,
		invitationID, account.ID)
	if err != nil {
		return User{}, fmt.Errorf("claim the invitation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return User{}, fmt.Errorf("claim the invitation: %w", err)
	}
	if affected == 0 {
		// Another acceptance won the row, or it stopped being usable between the
		// read and the claim. The inserted account goes with the rollback.
		return User{}, ErrInvalidToken
	}

	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit the invitation acceptance: %w", err)
	}
	return user, nil
}

// mapInsertError turns a uniqueness violation into the sentinel a caller can act
// on, and leaves every other failure wrapped so an operator can see it.
func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch {
		case strings.Contains(pgErr.ConstraintName, "username"):
			return ErrUsernameTaken
		case strings.Contains(pgErr.ConstraintName, "email"):
			return ErrEmailTaken
		}
	}
	return fmt.Errorf("create the administrator: %w", err)
}
