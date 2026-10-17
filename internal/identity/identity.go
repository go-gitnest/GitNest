package identity

import (
	"context"
	"errors"
	"time"
)

// Status describes whether an account may sign in.
type Status string

const (
	// StatusActive is a usable account.
	StatusActive Status = "active"
	// StatusDisabled is an account an administrator turned off. Existing
	// sessions stop resolving and new sign-ins are refused.
	StatusDisabled Status = "disabled"
)

// User describes a GitNest account.
//
// Password material is deliberately absent: this type is passed to transports
// and rendered in responses, and a stored hash must never travel with it.
type User struct {
	ID        string
	Username  string
	Email     string
	Status    Status
	IsAdmin   bool
	CreatedAt time.Time
}

// Session describes a signed-in browser session. The raw token and the CSRF
// token are returned once, when the session is issued, and are never stored in
// this form.
type Session struct {
	ID        string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Invitation authorizes one account to be created for one email address.
type Invitation struct {
	ID        string
	Email     string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
}

var (
	// ErrAdminExists reports that the instance already has an administrator.
	ErrAdminExists = errors.New("instance already has an administrator")
	// ErrSchemaNotMigrated reports that the account tables are missing, so the
	// operator has to migrate before continuing.
	ErrSchemaNotMigrated = errors.New("database schema is not migrated")
	// ErrUsernameTaken reports that the username is in use.
	ErrUsernameTaken = errors.New("username is already taken")
	// ErrEmailTaken reports that the email address is in use.
	ErrEmailTaken = errors.New("email address is already in use")
	// ErrInvalidCredentials reports a failed sign-in. It deliberately does not
	// distinguish an unknown account from a wrong password
	// (docs/security/threat-model.md, T-K2).
	ErrInvalidCredentials = errors.New("invalid username or password")
	// ErrAccountDisabled reports a sign-in attempt on a disabled account.
	ErrAccountDisabled = errors.New("account is disabled")
	// ErrNotAdmin reports an action that requires administrator rights.
	ErrNotAdmin = errors.New("administrator rights are required")
	// ErrInvalidToken reports a session or invitation token that is unknown,
	// malformed, expired, revoked, or already used. The cases are not
	// distinguished, so a caller cannot learn anything from the difference.
	ErrInvalidToken = errors.New("token is not valid")
	// ErrRateLimited reports that too many attempts failed in the window.
	ErrRateLimited = errors.New("too many attempts; try again later")
)

// NewAdmin is a fully prepared administrator for the store to persist.
type NewAdmin struct {
	ID             string
	Username       string
	UsernameFolded string
	Email          string
	PasswordHash   string
}

// NewAccount is a fully prepared non-bootstrap account, created by accepting an
// invitation.
type NewAccount struct {
	ID             string
	Username       string
	UsernameFolded string
	Email          string
	PasswordHash   string
}

// NewSession is a prepared session row. Only digests are stored.
type NewSession struct {
	ID            string
	UserID        string
	TokenHash     []byte
	CSRFTokenHash []byte
	ExpiresAt     time.Time
}

// NewInvitation is a prepared invitation row. Only the token digest is stored.
type NewInvitation struct {
	ID        string
	Email     string
	TokenHash []byte
	CreatedBy string
	ExpiresAt time.Time
}

// CreateInitialAdminInput is the operator-supplied bootstrap input, before
// normalization, validation, and hashing.
type CreateInitialAdminInput struct {
	Username string
	Email    string
	Password string
}

// CreateInvitationInput is the administrator-supplied invitation input.
type CreateInvitationInput struct {
	// ActorID is the administrator creating the invitation.
	ActorID string
	Email   string
}

// AcceptInvitationInput is the invitee-supplied input that creates an account.
type AcceptInvitationInput struct {
	// Token is the raw invitation token from the invitation link.
	Token    string
	Username string
	Password string
}

// LoginInput is one sign-in attempt.
type LoginInput struct {
	// Login accepts either the username or the email address.
	Login    string
	Password string
	// Source identifies the client for throttling, normally a remote address.
	// It never contributes to authentication.
	Source string
}

// Credentials is what a successful sign-in hands to a transport. The raw tokens
// are returned once; only their digests reach storage.
type Credentials struct {
	User         User
	Session      Session
	SessionToken string
	CSRFToken    string
}

// Store persists accounts, sessions, and invitations. The interface is owned
// here rather than by the database package, so the service depends on the
// behaviour it needs instead of on PostgreSQL.
type Store interface {
	// CreateInitialAdmin creates the instance's first administrator. It returns
	// ErrAdminExists when an administrator is already present,
	// ErrSchemaNotMigrated when the account schema is missing, and
	// ErrUsernameTaken or ErrEmailTaken when the details are in use.
	CreateInitialAdmin(ctx context.Context, admin NewAdmin) (User, error)

	// FindUserForLogin returns the account and its stored password hash for a
	// username or email address. It returns ErrInvalidCredentials when no
	// account matches, so an unknown account and a wrong password are
	// indistinguishable to the caller.
	FindUserForLogin(ctx context.Context, login string) (User, string, error)

	// FindUserByID returns one account, or ErrInvalidCredentials when it does
	// not exist.
	FindUserByID(ctx context.Context, id string) (User, error)

	// FindUserByUsername returns one account by its folded username, or
	// ErrInvalidCredentials when there is none. Resolution of an owner segment
	// uses it, so a route can turn `{owner}` into an account without reaching
	// for the credential-bearing sign-in path.
	FindUserByUsername(ctx context.Context, usernameFolded string) (User, error)

	// CreateSession stores a new session.
	CreateSession(ctx context.Context, session NewSession) (Session, error)

	// FindSessionByToken returns the account, the session row, and the session's
	// CSRF token digest for a token digest, reporting ErrInvalidToken when the
	// session is unknown, expired, or revoked, or its account is not active.
	FindSessionByToken(ctx context.Context, tokenHash []byte) (User, Session, []byte, error)

	// RevokeSessionByToken revokes one session, reporting ErrInvalidToken when
	// no live session matches.
	RevokeSessionByToken(ctx context.Context, tokenHash []byte) error

	// RevokeSessionsForUser revokes every live session of one account. It is
	// used on logout everywhere, password change, account disable, and
	// administrator-initiated revocation.
	RevokeSessionsForUser(ctx context.Context, userID string) error

	// UpdateSessionCSRFTokenHash replaces the CSRF token digest of a live
	// session, which is how the server-rendered surfaces issue a token to the
	// document they produce. Only the digest is stored.
	UpdateSessionCSRFTokenHash(ctx context.Context, sessionID string, csrfTokenHash []byte) error

	// CreateToken stores a new personal access token.
	CreateToken(ctx context.Context, token NewTokenRecord) (Token, error)

	// FindTokenByHash resolves a token digest to the account that owns it, which is
	// what turns a presented credential into a principal. It reports ErrInvalidToken
	// for a digest that is absent or revoked, so the two are indistinguishable.
	FindTokenByHash(ctx context.Context, tokenHash []byte) (User, Token, error)

	// ListTokens returns an account's tokens, live or revoked, newest first. It is what
	// the settings surface renders and what the administrator revoke command resolves a
	// prefix against.
	ListTokens(ctx context.Context, userID string) ([]Token, error)

	// RevokeToken stops one token working. It reports whether a live token was revoked,
	// so a caller can distinguish revoking from re-revoking.
	RevokeToken(ctx context.Context, userID, tokenID string) (bool, error)

	// RevokeTokenByPrefix revokes a live token named by its display prefix, which is the
	// administrator path in docs/architecture/administration.md. It reports how many
	// live tokens were revoked, so an ambiguous prefix is reported rather than guessed at.
	RevokeTokenByPrefix(ctx context.Context, prefix string) (int, error)

	// CreateInvitation stores a new single-use invitation.
	CreateInvitation(ctx context.Context, invitation NewInvitation) (Invitation, error)

	// FindInvitationByToken returns the invitation for a token digest,
	// reporting ErrInvalidToken when it is unknown, expired, accepted, or
	// revoked.
	FindInvitationByToken(ctx context.Context, tokenHash []byte) (Invitation, error)

	// AcceptInvitation creates an account from an invitation and marks the
	// invitation accepted in one transaction. It returns ErrInvalidToken when
	// the invitation stopped being usable, and ErrUsernameTaken or
	// ErrEmailTaken when the account details are in use.
	AcceptInvitation(ctx context.Context, invitationID string, account NewAccount) (User, error)
}
