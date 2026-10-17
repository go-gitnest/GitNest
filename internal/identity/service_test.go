package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore is an in-memory Store that applies the same rules as the PostgreSQL
// implementation where those rules are not the point of the test: expiry,
// revocation, account status, and single-use claims.
type fakeStore struct {
	mu          sync.Mutex
	users       map[string]*fakeUser
	sessions    map[string]*fakeSession    // keyed by token digest
	invitations map[string]*fakeInvitation // keyed by token digest
	tokens      map[string]*fakeToken      // keyed by token identifier

	createAdminErr error
	createAdminGot NewAdmin
	createAdminN   int
}

type fakeUser struct {
	user User
	hash string
}

type fakeSession struct {
	session  Session
	csrfHash []byte
	revoked  bool
}

type fakeInvitation struct {
	invitation Invitation
	accepted   bool
	revoked    bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:       map[string]*fakeUser{},
		sessions:    map[string]*fakeSession{},
		invitations: map[string]*fakeInvitation{},
		tokens:      map[string]*fakeToken{},
	}
}

// fakeToken is one stored token, with the digest that authenticates it.
type fakeToken struct {
	token     Token
	tokenHash []byte
	revoked   bool
	revokedAt time.Time
}

func (f *fakeStore) CreateInitialAdmin(_ context.Context, admin NewAdmin) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createAdminN++
	f.createAdminGot = admin
	if f.createAdminErr != nil {
		return User{}, f.createAdminErr
	}
	user := User{
		ID: admin.ID, Username: admin.Username, Email: admin.Email,
		Status: StatusActive, IsAdmin: true, CreatedAt: time.Unix(0, 0).UTC(),
	}
	f.users[user.ID] = &fakeUser{user: user, hash: admin.PasswordHash}
	return user, nil
}

func (f *fakeStore) FindUserForLogin(_ context.Context, login string) (User, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.users {
		if FoldName(record.user.Username) == login || record.user.Email == login {
			return record.user, record.hash, nil
		}
	}
	return User{}, "", ErrInvalidCredentials
}

func (f *fakeStore) FindUserByID(_ context.Context, id string) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, found := f.users[id]
	if !found {
		return User{}, ErrInvalidCredentials
	}
	return record.user, nil
}

func (f *fakeStore) FindUserByUsername(_ context.Context, usernameFolded string) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.users {
		if FoldName(record.user.Username) == usernameFolded {
			return record.user, nil
		}
	}
	return User{}, ErrInvalidCredentials
}

func (f *fakeStore) CreateSession(_ context.Context, session NewSession) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	created := Session{ID: session.ID, UserID: session.UserID, CreatedAt: time.Unix(0, 0).UTC(), ExpiresAt: session.ExpiresAt}
	f.sessions[string(session.TokenHash)] = &fakeSession{session: created, csrfHash: session.CSRFTokenHash}
	return created, nil
}

func (f *fakeStore) FindSessionByToken(_ context.Context, tokenHash []byte) (User, Session, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, found := f.sessions[string(tokenHash)]
	if !found || record.revoked || !record.session.ExpiresAt.After(time.Now()) {
		return User{}, Session{}, nil, ErrInvalidToken
	}
	owner, found := f.users[record.session.UserID]
	if !found || owner.user.Status != StatusActive {
		return User{}, Session{}, nil, ErrInvalidToken
	}
	return owner.user, record.session, record.csrfHash, nil
}

func (f *fakeStore) RevokeSessionByToken(_ context.Context, tokenHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, found := f.sessions[string(tokenHash)]
	if !found || record.revoked {
		return ErrInvalidToken
	}
	record.revoked = true
	return nil
}

func (f *fakeStore) RevokeSessionsForUser(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.sessions {
		if record.session.UserID == userID {
			record.revoked = true
		}
	}
	return nil
}

// UpdateSessionCSRFTokenHash mirrors the SQL: only a live session is updated, and
// the digest it is given becomes the one a resolution returns.
func (f *fakeStore) UpdateSessionCSRFTokenHash(_ context.Context, sessionID string, csrfTokenHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.sessions {
		if record.session.ID != sessionID {
			continue
		}
		if record.revoked {
			return ErrInvalidToken
		}
		record.csrfHash = csrfTokenHash
		return nil
	}
	return ErrInvalidToken
}

func (f *fakeStore) CreateInvitation(_ context.Context, invitation NewInvitation) (Invitation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	created := Invitation{
		ID: invitation.ID, Email: invitation.Email, CreatedBy: invitation.CreatedBy,
		CreatedAt: time.Unix(0, 0).UTC(), ExpiresAt: invitation.ExpiresAt,
	}
	f.invitations[string(invitation.TokenHash)] = &fakeInvitation{invitation: created}
	return created, nil
}

func (f *fakeStore) FindInvitationByToken(_ context.Context, tokenHash []byte) (Invitation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, found := f.invitations[string(tokenHash)]
	if !found || record.accepted || record.revoked || !record.invitation.ExpiresAt.After(time.Now()) {
		return Invitation{}, ErrInvalidToken
	}
	return record.invitation, nil
}

func (f *fakeStore) AcceptInvitation(_ context.Context, invitationID string, account NewAccount) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.invitations {
		if record.invitation.ID != invitationID {
			continue
		}
		if record.accepted || record.revoked || !record.invitation.ExpiresAt.After(time.Now()) {
			return User{}, ErrInvalidToken
		}
		record.accepted = true
		// The address comes from the invitation, never from the request.
		user := User{
			ID: account.ID, Username: account.Username, Email: record.invitation.Email,
			Status: StatusActive, CreatedAt: time.Unix(0, 0).UTC(),
		}
		f.users[user.ID] = &fakeUser{user: user, hash: account.PasswordHash}
		return user, nil
	}
	return User{}, ErrInvalidToken
}

// seedUser adds an account with a real password hash.
func (f *fakeStore) seedUser(t *testing.T, username, email string, isAdmin bool) User {
	t.Helper()
	hash, err := Argon2idHasher{}.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash the seeded password: %v", err)
	}
	user := User{
		ID: uuid.NewString(), Username: username, Email: email,
		Status: StatusActive, IsAdmin: isAdmin, CreatedAt: time.Unix(0, 0).UTC(),
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[user.ID] = &fakeUser{user: user, hash: hash}
	return user
}

// setStatus changes a seeded account's status.
func (f *fakeStore) setStatus(id string, status Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if record, found := f.users[id]; found {
		record.user.Status = status
	}
}

// expireSession ages the only session of an account.
func (f *fakeStore) expireSession() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range f.sessions {
		record.session.ExpiresAt = time.Now().Add(-time.Minute)
	}
}

// seeded returns a service with a member and an administrator account.
func seeded(t *testing.T) (*Service, *fakeStore, User, User) {
	t.Helper()
	store := newFakeStore()
	admin := store.seedUser(t, "root", "root@example.com", true)
	member := store.seedUser(t, "member", "member@example.com", false)
	return NewService(store, Argon2idHasher{}), store, admin, member
}

func TestCreateInitialAdminValidatesAndHashes(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, Argon2idHasher{})

	user, err := service.CreateInitialAdmin(context.Background(), CreateInitialAdminInput{
		Username: "  Alice  ", Email: " Alice@Example.COM ", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("CreateInitialAdmin: %v", err)
	}
	if store.createAdminN != 1 {
		t.Fatalf("store calls = %d, want 1", store.createAdminN)
	}
	if store.createAdminGot.Username != "Alice" || store.createAdminGot.UsernameFolded != "alice" {
		t.Errorf("stored %q/%q, want the display form and its folded form",
			store.createAdminGot.Username, store.createAdminGot.UsernameFolded)
	}
	if store.createAdminGot.Email != "alice@example.com" {
		t.Errorf("stored email = %q, want the normalized form", store.createAdminGot.Email)
	}
	// The plaintext must never reach storage.
	if store.createAdminGot.PasswordHash == testPassword {
		t.Fatal("the store received the plaintext password")
	}
	ok, err := VerifyPassword(store.createAdminGot.PasswordHash, testPassword)
	if err != nil || !ok {
		t.Errorf("the stored hash does not verify the password (err=%v, ok=%t)", err, ok)
	}
	if _, err := uuid.Parse(store.createAdminGot.ID); err != nil {
		t.Errorf("stored id %q is not a UUID: %v", store.createAdminGot.ID, err)
	}
	if !user.IsAdmin {
		t.Error("the created account is not an administrator")
	}
}

// TestCreateInitialAdminRejectsInvalidInputBeforeHashing is the ordering
// guarantee: an invalid request must not cost a full Argon2id computation.
func TestCreateInitialAdminRejectsInvalidInputBeforeHashing(t *testing.T) {
	for _, test := range []struct {
		name  string
		input CreateInitialAdminInput
	}{
		{name: "empty username", input: CreateInitialAdminInput{Username: "  ", Email: "a@example.com", Password: testPassword}},
		{name: "reserved username", input: CreateInitialAdminInput{Username: "admin", Email: "a@example.com", Password: testPassword}},
		{name: "username with a dot", input: CreateInitialAdminInput{Username: "a.b", Email: "a@example.com", Password: testPassword}},
		{name: "invalid email", input: CreateInitialAdminInput{Username: "alice", Email: "nope", Password: testPassword}},
		{name: "short password", input: CreateInitialAdminInput{Username: "alice", Email: "a@example.com", Password: "short"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newFakeStore()
			if _, err := NewService(store, Argon2idHasher{}).CreateInitialAdmin(context.Background(), test.input); err == nil {
				t.Fatal("invalid input was accepted")
			}
			if store.createAdminN != 0 {
				t.Errorf("the store was called %d times for invalid input", store.createAdminN)
			}
		})
	}
}

func TestCreateInitialAdminPropagatesStoreErrors(t *testing.T) {
	store := newFakeStore()
	store.createAdminErr = ErrAdminExists
	_, err := NewService(store, Argon2idHasher{}).CreateInitialAdmin(context.Background(), CreateInitialAdminInput{
		Username: "alice", Email: "alice@example.com", Password: testPassword,
	})
	if !errors.Is(err, ErrAdminExists) {
		t.Fatalf("error = %v, want ErrAdminExists", err)
	}
}

func TestNewIDIsATimeOrderedUUID(t *testing.T) {
	first, second := newID(), newID()
	if first == second {
		t.Fatal("two identifiers were identical")
	}
	parsed, err := uuid.Parse(first)
	if err != nil {
		t.Fatalf("identifier %q is not a UUID: %v", first, err)
	}
	if parsed.Version() != 7 {
		t.Errorf("identifier version = %d, want 7 for a time-ordered UUID", parsed.Version())
	}
}

// TestLoginAcceptsUsernameOrEmail covers both sign-in forms, case-insensitively.
func TestLoginAcceptsUsernameOrEmail(t *testing.T) {
	service, _, _, member := seeded(t)

	for _, login := range []string{"member", "MEMBER", "member@example.com", " Member@Example.com "} {
		t.Run(login, func(t *testing.T) {
			credentials, err := service.Login(context.Background(), LoginInput{Login: login, Password: testPassword})
			if err != nil {
				t.Fatalf("Login(%q): %v", login, err)
			}
			if credentials.User.ID != member.ID {
				t.Errorf("signed in as %q, want the member account", credentials.User.Username)
			}
			if credentials.SessionToken == "" || credentials.CSRFToken == "" {
				t.Fatal("Login returned an empty token")
			}
			if credentials.SessionToken == credentials.CSRFToken {
				t.Error("the session token and the CSRF token are the same value")
			}
		})
	}
}

func TestLoginRejectsBadCredentialsGenerically(t *testing.T) {
	service, _, _, _ := seeded(t)

	for _, test := range []struct {
		name  string
		input LoginInput
	}{
		{name: "wrong password", input: LoginInput{Login: "member", Password: "not the password"}},
		{name: "unknown account", input: LoginInput{Login: "ghost", Password: testPassword}},
		{name: "empty login", input: LoginInput{Login: "", Password: testPassword}},
		{name: "empty password", input: LoginInput{Login: "member", Password: ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Login(context.Background(), test.input)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("error = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

func TestLoginRefusesADisabledAccount(t *testing.T) {
	service, store, _, member := seeded(t)
	store.setStatus(member.ID, StatusDisabled)

	_, err := service.Login(context.Background(), LoginInput{Login: "member", Password: testPassword})
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("error = %v, want ErrAccountDisabled", err)
	}
}

// TestLoginThrottlesFailuresOnly pins the documented rule: the limiter stops
// guessing without locking out a user who mistypes.
func TestLoginThrottlesFailuresOnly(t *testing.T) {
	service, _, _, _ := seeded(t)
	service.WithThrottling(3, 3)
	ctx := context.Background()

	// The first two failures report the credentials problem itself.
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := service.Login(ctx, LoginInput{Login: "member", Password: "wrong", Source: "10.0.0.1"})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", attempt, err)
		}
	}
	// The failure that reaches the limit is itself reported as throttling, so
	// the caller learns to slow down at the moment the limit is hit.
	if _, err := service.Login(ctx, LoginInput{Login: "member", Password: "wrong", Source: "10.0.0.1"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("attempt at the limit = %v, want ErrRateLimited", err)
	}

	// A correct password is never refused, and it clears the window.
	if _, err := service.Login(ctx, LoginInput{Login: "member", Password: testPassword, Source: "10.0.0.1"}); err != nil {
		t.Fatalf("the correct password was refused: %v", err)
	}
	if _, err := service.Login(ctx, LoginInput{Login: "member", Password: "wrong", Source: "10.0.0.1"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("after the reset = %v, want the credentials error again", err)
	}

	// A failure from another source does not affect this one.
	if _, err := service.Login(ctx, LoginInput{Login: "member", Password: testPassword, Source: "10.0.0.2"}); err != nil {
		t.Fatalf("another source was affected: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	service, _, _, member := seeded(t)
	ctx := context.Background()

	credentials, err := service.Login(ctx, LoginInput{Login: "member", Password: testPassword})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	user, session, csrfDigest, err := service.ResolveSession(ctx, credentials.SessionToken)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if user.ID != member.ID || session.UserID != member.ID {
		t.Errorf("resolved %q/%q, want the member account", user.ID, session.UserID)
	}
	if !DigestsMatch(HashToken(credentials.CSRFToken), csrfDigest) {
		t.Error("the session does not carry the CSRF token's digest")
	}

	if err := service.Logout(ctx, credentials.SessionToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := service.CurrentUser(ctx, credentials.SessionToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("after logout = %v, want ErrInvalidToken", err)
	}
	// A replayed logout reports the token is no longer live.
	if err := service.Logout(ctx, credentials.SessionToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second Logout = %v, want ErrInvalidToken", err)
	}
}

// TestExpiredSessionFailsLikeAnUnknownOne pins that the reason is not disclosed.
func TestExpiredSessionFailsLikeAnUnknownOne(t *testing.T) {
	service, store, _, _ := seeded(t)
	ctx := context.Background()

	credentials, err := service.Login(ctx, LoginInput{Login: "member", Password: testPassword})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	store.expireSession()

	_, expired := service.CurrentUser(ctx, credentials.SessionToken)
	_, unknown := service.CurrentUser(ctx, "no-such-token")
	if !errors.Is(expired, ErrInvalidToken) || !errors.Is(unknown, ErrInvalidToken) {
		t.Fatalf("expired = %v, unknown = %v; both must be ErrInvalidToken", expired, unknown)
	}
	if expired.Error() != unknown.Error() {
		t.Errorf("the two failures differ: %q vs %q", expired, unknown)
	}
}

// TestDisablingAnAccountStopsItsSessions covers the account-status rule.
func TestDisablingAnAccountStopsItsSessions(t *testing.T) {
	service, store, _, member := seeded(t)
	ctx := context.Background()

	credentials, err := service.Login(ctx, LoginInput{Login: "member", Password: testPassword})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	store.setStatus(member.ID, StatusDisabled)

	if _, err := service.CurrentUser(ctx, credentials.SessionToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("after disabling = %v, want ErrInvalidToken", err)
	}
	if _, err := service.Login(ctx, LoginInput{Login: "member", Password: testPassword}); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("sign-in after disabling = %v, want ErrAccountDisabled", err)
	}
}

func TestCurrentUserRejectsMalformedTokens(t *testing.T) {
	service, _, _, _ := seeded(t)
	ctx := context.Background()

	for _, token := range []string{"", "not-a-token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := service.CurrentUser(ctx, token); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("CurrentUser(%q) = %v, want ErrInvalidToken", token, err)
		}
	}
}

func TestRevokeAllSessionsInvalidatesEverySession(t *testing.T) {
	service, _, _, member := seeded(t)
	ctx := context.Background()

	var tokens []string
	for i := 0; i < 3; i++ {
		credentials, err := service.Login(ctx, LoginInput{Login: "member", Password: testPassword})
		if err != nil {
			t.Fatalf("Login %d: %v", i, err)
		}
		tokens = append(tokens, credentials.SessionToken)
	}

	if err := service.RevokeAllSessions(ctx, member.ID); err != nil {
		t.Fatalf("RevokeAllSessions: %v", err)
	}
	for i, token := range tokens {
		if _, err := service.CurrentUser(ctx, token); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("session %d still resolves after revocation", i)
		}
	}
}

func TestCreateInvitationRequiresAnActiveAdministrator(t *testing.T) {
	service, store, admin, member := seeded(t)
	ctx := context.Background()

	if _, _, err := service.CreateInvitation(ctx, CreateInvitationInput{ActorID: member.ID, Email: "new@example.com"}); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("a member created an invitation: %v", err)
	}
	store.setStatus(admin.ID, StatusDisabled)
	if _, _, err := service.CreateInvitation(ctx, CreateInvitationInput{ActorID: admin.ID, Email: "new@example.com"}); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("a disabled administrator created an invitation: %v", err)
	}
}

func TestCreateInvitationStoresOnlyTheDigest(t *testing.T) {
	service, store, admin, _ := seeded(t)
	ctx := context.Background()

	invitation, token, err := service.CreateInvitation(ctx, CreateInvitationInput{
		ActorID: admin.ID, Email: " Newcomer@Example.COM ",
	})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	if invitation.Email != "newcomer@example.com" {
		t.Errorf("invitation email = %q, want the normalized form", invitation.Email)
	}
	if token == "" {
		t.Fatal("CreateInvitation returned an empty token")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	stored, found := store.invitations[string(HashToken(token))]
	if !found {
		t.Fatal("the invitation was stored under its raw token, not its digest")
	}
	if stored.invitation.ExpiresAt.Before(time.Now()) {
		t.Error("the invitation is already expired")
	}
	for digest := range store.invitations {
		if digest == token {
			t.Error("the raw token was stored as the key")
		}
	}
}

func TestCreateInvitationRejectsAnInvalidAddress(t *testing.T) {
	service, _, admin, _ := seeded(t)
	if _, _, err := service.CreateInvitation(context.Background(), CreateInvitationInput{
		ActorID: admin.ID, Email: "not-an-address",
	}); err == nil {
		t.Fatal("an invalid address was accepted")
	}
}

// TestAcceptInvitationUsesTheInvitationAddress is the rule that the invitee
// cannot redirect an invitation to another address.
func TestAcceptInvitationUsesTheInvitationAddress(t *testing.T) {
	service, _, admin, _ := seeded(t)
	ctx := context.Background()

	_, token, err := service.CreateInvitation(ctx, CreateInvitationInput{ActorID: admin.ID, Email: "newcomer@example.com"})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	user, err := service.AcceptInvitation(ctx, AcceptInvitationInput{
		Token: token, Username: "Newcomer", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if user.Email != "newcomer@example.com" {
		t.Errorf("created %q, want the invitation's address", user.Email)
	}
	if user.Username != "Newcomer" {
		t.Errorf("created %q, want the display form preserved", user.Username)
	}
	if user.IsAdmin {
		t.Error("an invited account was created as an administrator")
	}

	// The account can sign in.
	if _, err := service.Login(ctx, LoginInput{Login: "newcomer", Password: testPassword}); err != nil {
		t.Fatalf("the invited account cannot sign in: %v", err)
	}
}

func TestAcceptInvitationIsSingleUse(t *testing.T) {
	service, _, admin, _ := seeded(t)
	ctx := context.Background()

	_, token, err := service.CreateInvitation(ctx, CreateInvitationInput{ActorID: admin.ID, Email: "once@example.com"})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	if _, err := service.AcceptInvitation(ctx, AcceptInvitationInput{Token: token, Username: "first", Password: testPassword}); err != nil {
		t.Fatalf("first acceptance: %v", err)
	}
	if _, err := service.AcceptInvitation(ctx, AcceptInvitationInput{Token: token, Username: "second", Password: testPassword}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replayed acceptance = %v, want ErrInvalidToken", err)
	}
}

func TestAcceptInvitationRejectsBadInput(t *testing.T) {
	service, _, admin, _ := seeded(t)
	ctx := context.Background()

	_, token, err := service.CreateInvitation(ctx, CreateInvitationInput{ActorID: admin.ID, Email: "newcomer@example.com"})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	for _, test := range []struct {
		name  string
		input AcceptInvitationInput
	}{
		{name: "empty token", input: AcceptInvitationInput{Token: "", Username: "newcomer", Password: testPassword}},
		{name: "unknown token", input: AcceptInvitationInput{Token: "unknown", Username: "newcomer", Password: testPassword}},
		{name: "invalid username", input: AcceptInvitationInput{Token: token, Username: "a.b", Password: testPassword}},
		{name: "short password", input: AcceptInvitationInput{Token: token, Username: "newcomer", Password: "short"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.AcceptInvitation(ctx, test.input); err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
	}
}

// TestAcceptInvitationThrottlesFailuresOnly keeps the limit aimed at token
// guessing: a real invitation still works after a failed attempt.
func TestAcceptInvitationThrottlesFailuresOnly(t *testing.T) {
	service, _, admin, _ := seeded(t)
	service.WithThrottling(0, 2)
	ctx := context.Background()

	_, token, err := service.CreateInvitation(ctx, CreateInvitationInput{ActorID: admin.ID, Email: "newcomer@example.com"})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	if _, err := service.AcceptInvitation(ctx, AcceptInvitationInput{Token: "unknown", Username: "newcomer", Password: testPassword}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("bad token = %v, want ErrInvalidToken", err)
	}
	if _, err := service.AcceptInvitation(ctx, AcceptInvitationInput{Token: "unknown", Username: "newcomer", Password: testPassword}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second failure = %v, want ErrRateLimited", err)
	}
	// A valid invitation still works: the limit counts failures, not attempts.
	if _, err := service.AcceptInvitation(ctx, AcceptInvitationInput{Token: token, Username: "newcomer", Password: testPassword}); err != nil {
		t.Fatalf("a valid invitation was refused while throttled: %v", err)
	}
}

// The token methods mirror the SQL, so a service test exercises the same rules the
// database enforces: a digest lookup ignores revoked tokens, a revocation is scoped to its
// owner, and a prefix match reports every live token it covers.

func (f *fakeStore) CreateToken(_ context.Context, record NewTokenRecord) (Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	created := Token{
		ID:        record.ID,
		UserID:    record.UserID,
		Name:      record.Name,
		Prefix:    record.TokenPrefix,
		Scopes:    append([]string(nil), record.Scopes...),
		CreatedAt: time.Now().UTC(),
	}
	f.tokens[record.ID] = &fakeToken{
		token:     created,
		tokenHash: append([]byte(nil), record.TokenHash...),
	}
	return created, nil
}

func (f *fakeStore) FindTokenByHash(_ context.Context, tokenHash []byte) (User, Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, stored := range f.tokens {
		if stored.revoked || !DigestsMatch(stored.tokenHash, tokenHash) {
			continue
		}
		// The account is resolved from the token, exactly as the join does.
		for _, account := range f.users {
			if account.user.ID != stored.token.UserID {
				continue
			}
			if account.user.Status != StatusActive {
				return User{}, Token{}, ErrAccountDisabled
			}
			return account.user, stored.token, nil
		}
	}
	return User{}, Token{}, ErrInvalidToken
}

func (f *fakeStore) ListTokens(_ context.Context, userID string) ([]Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	tokens := []Token{}
	for _, stored := range f.tokens {
		if stored.token.UserID != userID {
			continue
		}
		listed := stored.token
		if stored.revoked {
			at := stored.revokedAt
			listed.RevokedAt = &at
		}
		tokens = append(tokens, listed)
	}
	return tokens, nil
}

func (f *fakeStore) RevokeToken(_ context.Context, userID, tokenID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored, found := f.tokens[tokenID]
	// The scoping is the point: a token that belongs to another account is not found.
	if !found || stored.token.UserID != userID || stored.revoked {
		return false, nil
	}
	stored.revoked = true
	stored.revokedAt = time.Now().UTC()
	return true, nil
}

func (f *fakeStore) RevokeTokenByPrefix(_ context.Context, prefix string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	revoked := 0
	for _, stored := range f.tokens {
		if stored.revoked || stored.token.Prefix != prefix {
			continue
		}
		stored.revoked = true
		stored.revokedAt = time.Now().UTC()
		revoked++
	}
	return revoked, nil
}
