package identity

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Session and invitation defaults, in days. Both are instance settings in
// docs/architecture/configuration.md, so the registry supplies them once it
// exists; these values are what it starts from.
const (
	DefaultSessionLifetime    = 30 * 24 * time.Hour
	DefaultInvitationLifetime = 7 * 24 * time.Hour
)

// Default failure thresholds, keyed per source and per window.
const (
	DefaultLoginFailureLimit      = 5
	DefaultInvitationFailureLimit = 10
	DefaultThrottleWindow         = 15 * time.Minute
)

// invitationSource keys the invitation limiter. Acceptance is throttled
// process-wide because the domain service has no client identity; a transport
// that knows the caller should key on it instead.
const invitationSource = "invitation"

// Service holds the account use cases that every transport calls.
type Service struct {
	store  Store
	hasher Hasher
	now    func() time.Time

	sessionLifetime    time.Duration
	invitationLifetime time.Duration

	// loginLimiter throttles failed sign-ins per source. A nil limiter allows
	// everything, which keeps a test that is not about throttling free of setup.
	loginLimiter *RateLimiter
	// invitationLimiter throttles failed invitation acceptances.
	invitationLimiter *RateLimiter
}

// NewService wires the service to its storage and its password hasher, with the
// documented default lifetimes.
func NewService(store Store, hasher Hasher) *Service {
	return &Service{
		store:              store,
		hasher:             hasher,
		now:                time.Now,
		sessionLifetime:    DefaultSessionLifetime,
		invitationLifetime: DefaultInvitationLifetime,
	}
}

// WithLifetimes overrides the session and invitation lifetimes, which the
// instance settings registry supplies.
func (s *Service) WithLifetimes(session, invitation time.Duration) *Service {
	if session > 0 {
		s.sessionLifetime = session
	}
	if invitation > 0 {
		s.invitationLifetime = invitation
	}
	return s
}

// WithThrottling enables failure-only throttling for sign-in and invitation
// acceptance, at the documented thresholds.
func (s *Service) WithThrottling(loginLimit, invitationLimit int) *Service {
	window := DefaultThrottleWindow
	s.loginLimiter = NewRateLimiter(loginLimit, window)
	s.invitationLimiter = NewRateLimiter(invitationLimit, window)
	return s
}

// CreateInitialAdmin validates the operator's input, hashes the password, and
// asks the store to create the instance's only bootstrap administrator.
//
// Validation runs before hashing, so an invalid account never reaches the
// database and never costs a full Argon2id computation.
func (s *Service) CreateInitialAdmin(ctx context.Context, input CreateInitialAdminInput) (User, error) {
	username := strings.TrimSpace(input.Username)
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	email := NormalizeEmail(input.Email)
	if err := ValidateEmail(email); err != nil {
		return User{}, err
	}
	if err := ValidatePassword(input.Password); err != nil {
		return User{}, err
	}

	hash, err := s.hasher.Hash(input.Password)
	if err != nil {
		return User{}, err
	}
	return s.store.CreateInitialAdmin(ctx, NewAdmin{
		ID:             newID(),
		Username:       username,
		UsernameFolded: FoldName(username),
		Email:          email,
		PasswordHash:   hash,
	})
}

// Login verifies a username or email and password, and issues a session.
//
// Every failure other than throttling reports ErrInvalidCredentials, so a
// caller cannot tell an unknown account from a wrong password. The limiter is
// consulted only for a failed attempt, so a correct password is never refused.
func (s *Service) Login(ctx context.Context, input LoginInput) (Credentials, error) {
	login := strings.TrimSpace(input.Login)
	if login == "" || input.Password == "" {
		return Credentials{}, s.rejectLogin(input.Source)
	}

	user, hash, err := s.store.FindUserForLogin(ctx, FoldName(login))
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return Credentials{}, s.rejectLogin(input.Source)
		}
		return Credentials{}, err
	}

	// The password is verified even for a disabled account, so a disabled
	// account does not answer faster than an enabled one.
	ok, err := VerifyPassword(hash, input.Password)
	if err != nil || !ok {
		return Credentials{}, s.rejectLogin(input.Source)
	}
	if user.Status != StatusActive {
		return Credentials{}, ErrAccountDisabled
	}

	sessionToken, tokenDigest, err := NewToken()
	if err != nil {
		return Credentials{}, err
	}
	csrfToken, csrfDigest, err := NewToken()
	if err != nil {
		return Credentials{}, err
	}
	session, err := s.store.CreateSession(ctx, NewSession{
		ID:            newID(),
		UserID:        user.ID,
		TokenHash:     tokenDigest,
		CSRFTokenHash: csrfDigest,
		ExpiresAt:     s.now().Add(s.sessionLifetime),
	})
	if err != nil {
		return Credentials{}, err
	}

	if s.loginLimiter != nil {
		s.loginLimiter.Reset(input.Source)
	}
	return Credentials{
		User:         user,
		Session:      session,
		SessionToken: sessionToken,
		CSRFToken:    csrfToken,
	}, nil
}

// rejectLogin records one failed attempt and returns the error to report. Once
// the source has used up its attempts the failure is reported as throttling,
// which is what a caller surfaces as "try again later".
func (s *Service) rejectLogin(source string) error {
	if s.loginLimiter == nil {
		return ErrInvalidCredentials
	}
	s.loginLimiter.Record(source)
	if s.loginLimiter.Blocked(source) {
		return ErrRateLimited
	}
	return ErrInvalidCredentials
}

// CurrentUser resolves a session token to its account, and reports the CSRF
// token digest the session carries so a transport can verify a state-changing
// request before the handler runs.
func (s *Service) CurrentUser(ctx context.Context, sessionToken string) (User, error) {
	user, _, _, err := s.ResolveSession(ctx, sessionToken)
	return user, err
}

// ResolveSession resolves a session token to its account, its session row, and
// its CSRF token digest.
func (s *Service) ResolveSession(ctx context.Context, sessionToken string) (User, Session, []byte, error) {
	if sessionToken == "" {
		return User{}, Session{}, nil, ErrInvalidToken
	}
	return s.store.FindSessionByToken(ctx, HashToken(sessionToken))
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, sessionToken string) error {
	if sessionToken == "" {
		return ErrInvalidToken
	}
	return s.store.RevokeSessionByToken(ctx, HashToken(sessionToken))
}

// RotateCSRFToken issues a fresh CSRF token for a live session and returns the
// token itself.
//
// The session already carries one CSRF digest, and only that digest is stored, so
// the token behind it cannot be recovered — which is what
// docs/architecture/secrets.md requires (a high-entropy token is kept as a
// SHA-256 digest) and what makes a database leak useless. The consequence is that
// a browser which has lost the token cannot be handed the same one again; it can
// only be given a new one, and that is what this does.
//
// It exists for the server-rendered surfaces. A shell page is produced for a
// session the server has already resolved, so it can issue the token that the
// document's script will attach to a state-changing request. Without this, a
// token could only be obtained from the sign-in response, so reloading a page
// would leave every form unable to submit.
//
// Issuing a token replaces the session's previous one, so at most one token is
// live per session at a time. Two documents open from the same session therefore
// share one live token, and the later document's is the valid one — the tradeoff
// of keeping a single digest, which is the storage the document prescribes.
// A request that presents the superseded token is refused like any other invalid
// token; the reader reloads and submits again.
//
// This is deliberately not a rotation on a timer and not a derivation from a
// server key: T-K4 requires a token "bound to the session", and a derived value
// would be computable from the session cookie, which would collapse the two
// independent secrets into one.
func (s *Service) RotateCSRFToken(ctx context.Context, sessionToken string) (string, error) {
	if sessionToken == "" {
		return "", ErrInvalidToken
	}
	_, session, _, err := s.ResolveSession(ctx, sessionToken)
	if err != nil {
		return "", err
	}

	token, digest, err := NewToken()
	if err != nil {
		return "", err
	}
	if err := s.store.UpdateSessionCSRFTokenHash(ctx, session.ID, digest); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeAllSessions revokes every live session of an account. It is the
// mechanism behind logout everywhere, password change, account disable, and
// administrator-initiated revocation.
func (s *Service) RevokeAllSessions(ctx context.Context, userID string) error {
	if userID == "" {
		return ErrInvalidCredentials
	}
	return s.store.RevokeSessionsForUser(ctx, userID)
}

// CreateInvitation lets an active administrator invite one email address, and
// returns the raw token exactly once for delivery.
func (s *Service) CreateInvitation(ctx context.Context, input CreateInvitationInput) (Invitation, string, error) {
	actor, err := s.store.FindUserByID(ctx, input.ActorID)
	if err != nil {
		return Invitation{}, "", err
	}
	if !actor.IsAdmin || actor.Status != StatusActive {
		return Invitation{}, "", ErrNotAdmin
	}

	email := NormalizeEmail(input.Email)
	if err := ValidateEmail(email); err != nil {
		return Invitation{}, "", err
	}

	token, digest, err := NewToken()
	if err != nil {
		return Invitation{}, "", err
	}
	invitation, err := s.store.CreateInvitation(ctx, NewInvitation{
		ID:        newID(),
		Email:     email,
		TokenHash: digest,
		CreatedBy: actor.ID,
		ExpiresAt: s.now().Add(s.invitationLifetime),
	})
	if err != nil {
		return Invitation{}, "", err
	}
	return invitation, token, nil
}

// AcceptInvitation creates an account from a valid invitation.
//
// The account's email always comes from the invitation row, never from the
// request, so an invitee cannot redirect an invitation to another address.
//
// Throttling applies to failures only: a mistyped username or password does not
// consume the invitee's chances to use a still-valid invitation.
func (s *Service) AcceptInvitation(ctx context.Context, input AcceptInvitationInput) (User, error) {
	reject := func(err error) (User, error) {
		if s.invitationLimiter == nil {
			return User{}, err
		}
		s.invitationLimiter.Record(invitationSource)
		if s.invitationLimiter.Blocked(invitationSource) {
			return User{}, ErrRateLimited
		}
		return User{}, err
	}

	if input.Token == "" {
		return reject(ErrInvalidToken)
	}
	invitation, err := s.store.FindInvitationByToken(ctx, HashToken(input.Token))
	if err != nil {
		return reject(err)
	}

	username := strings.TrimSpace(input.Username)
	if err := ValidateUsername(username); err != nil {
		return reject(err)
	}
	if err := ValidatePassword(input.Password); err != nil {
		return reject(err)
	}
	hash, err := s.hasher.Hash(input.Password)
	if err != nil {
		return User{}, err
	}

	user, err := s.store.AcceptInvitation(ctx, invitation.ID, NewAccount{
		ID:             newID(),
		Username:       username,
		UsernameFolded: FoldName(username),
		Email:          invitation.Email,
		PasswordHash:   hash,
	})
	if err != nil {
		return reject(err)
	}
	if s.invitationLimiter != nil {
		s.invitationLimiter.Reset(invitationSource)
	}
	return user, nil
}

// OwnerByUsername resolves the account a route's `{owner}` segment names.
//
// It validates the shape before it queries, so a malformed segment costs no
// database round trip and cannot be used to probe the schema. An unknown owner
// is reported like an unknown account, so a caller cannot distinguish the two.
//
// This is resolution, not authorization: the caller still has to pass the
// permission decision for the repository it resolves.
func (s *Service) OwnerByUsername(ctx context.Context, username string) (User, error) {
	trimmed := strings.TrimSpace(username)
	if err := ValidateUsername(trimmed); err != nil {
		return User{}, ErrInvalidCredentials
	}
	return s.store.FindUserByUsername(ctx, FoldName(trimmed))
}

// AccountByID resolves an account from its identifier.
//
// It exists for a caller that holds an identifier rather than a name — a
// collaborator grant stores the account, and a listing has to name it. An
// unknown identifier is reported like an unknown account, so the two cannot be
// told apart.
//
// This is resolution, not authorization: the caller still has to pass the
// permission decision for whatever it is about to do.
func (s *Service) AccountByID(ctx context.Context, id string) (User, error) {
	if strings.TrimSpace(id) == "" {
		return User{}, ErrInvalidCredentials
	}
	return s.store.FindUserByID(ctx, id)
}

// newID returns the identifier for a new row.
//
// A UUIDv7 is time-ordered, which keeps index inserts local, and PostgreSQL
// stores it natively as uuid (docs/architecture/data-model.md, open questions).
func newID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	// A random v4 is a fine fallback: it loses index locality, not correctness.
	return uuid.NewString()
}
