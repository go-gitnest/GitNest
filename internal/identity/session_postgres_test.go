package identity_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
)

// password satisfies the documented minimum length.
const password = "correct horse battery staple"

// integrationService returns a service on a migrated schema, the pool behind it
// for direct assertions, and the bootstrap administrator created through the
// real path.
func integrationService(t *testing.T) (*identity.Service, *sql.DB, identity.User) {
	t.Helper()
	schema := postgrestest.NewSchema(t)
	service := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	admin, err := service.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "root", Email: "root@example.com", Password: password,
	})
	if err != nil {
		t.Fatalf("bootstrap the administrator: %v", err)
	}
	return service, schema.Client.DB, admin
}

// inviteAndAccept drives the full invitation path and returns the new account.
func inviteAndAccept(t *testing.T, service *identity.Service, actorID, email, username string) identity.User {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, token, err := service.CreateInvitation(ctx, identity.CreateInvitationInput{ActorID: actorID, Email: email})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	user, err := service.AcceptInvitation(ctx, identity.AcceptInvitationInput{
		Token: token, Username: username, Password: password,
	})
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	return user
}

// TestInvitationAcceptanceEndToEnd covers the whole invited-contributor path.
func TestInvitationAcceptanceEndToEnd(t *testing.T) {
	service, _, admin := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	user := inviteAndAccept(t, service, admin.ID, "Newcomer@Example.com", "Newcomer")
	if user.Email != "newcomer@example.com" {
		t.Errorf("created %q, want the invitation's normalized address", user.Email)
	}
	if user.IsAdmin {
		t.Error("an invited account was created as an administrator")
	}

	credentials, err := service.Login(ctx, identity.LoginInput{Login: "newcomer", Password: password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	current, err := service.CurrentUser(ctx, credentials.SessionToken)
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if current.ID != user.ID {
		t.Errorf("resolved %q, want the invited account", current.Username)
	}
	if err := service.Logout(ctx, credentials.SessionToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := service.CurrentUser(ctx, credentials.SessionToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("after logout = %v, want ErrInvalidToken", err)
	}
}

// TestSessionStoresOnlyDigests proves the raw token never reaches the database.
func TestSessionStoresOnlyDigests(t *testing.T) {
	service, db, _ := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	credentials, err := service.Login(ctx, identity.LoginInput{Login: "root", Password: password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	var stored []byte
	if err := db.QueryRowContext(ctx,
		"SELECT token_hash FROM sessions WHERE id = $1", credentials.Session.ID).Scan(&stored); err != nil {
		t.Fatalf("read the stored digest: %v", err)
	}
	if string(stored) == credentials.SessionToken {
		t.Error("the raw session token was stored")
	}
	if len(stored) != 32 {
		t.Errorf("stored digest is %d bytes, want a SHA-256 digest", len(stored))
	}
	if !identity.DigestsMatch(identity.HashToken(credentials.SessionToken), stored) {
		t.Error("the stored digest does not match the issued token")
	}
}

// TestConcurrentInvitationAcceptanceCreatesOneAccount proves the transactional
// claim: two parallel acceptances of one token cannot both succeed.
func TestConcurrentInvitationAcceptanceCreatesOneAccount(t *testing.T) {
	service, _, admin := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, token, err := service.CreateInvitation(ctx, identity.CreateInvitationInput{
		ActorID: admin.ID, Email: "race@example.com",
	})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}

	const attempts = 4
	var (
		wait    sync.WaitGroup
		mu      sync.Mutex
		created int
		refused int
		other   []error
	)
	for i := 0; i < attempts; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			_, err := service.AcceptInvitation(ctx, identity.AcceptInvitationInput{
				Token: token, Username: fmt.Sprintf("racer%d", i), Password: password,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, identity.ErrInvalidToken):
				refused++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wait.Wait()

	for _, err := range other {
		t.Errorf("unexpected error: %v", err)
	}
	if created != 1 || refused != attempts-1 {
		t.Fatalf("created = %d, refused = %d; want 1 and %d", created, refused, attempts-1)
	}
}

func TestExpiredAndRevokedInvitationsFail(t *testing.T) {
	service, db, admin := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for _, test := range []struct {
		name   string
		expire bool
		revoke bool
	}{
		{name: "expired", expire: true},
		{name: "revoked", revoke: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			email := test.name + "@example.com"
			_, token, err := service.CreateInvitation(ctx, identity.CreateInvitationInput{ActorID: admin.ID, Email: email})
			if err != nil {
				t.Fatalf("CreateInvitation: %v", err)
			}

			// The invitation is aged or revoked through SQL, so the predicate
			// under test is the one the production query applies.
			if test.expire {
				if _, err := db.ExecContext(ctx,
					"UPDATE invitations SET expires_at = NOW() - INTERVAL '1 hour' WHERE email = $1", email); err != nil {
					t.Fatalf("expire the invitation: %v", err)
				}
			}
			if test.revoke {
				if _, err := db.ExecContext(ctx,
					"UPDATE invitations SET revoked_at = NOW() WHERE email = $1", email); err != nil {
					t.Fatalf("revoke the invitation: %v", err)
				}
			}

			if _, err := service.AcceptInvitation(ctx, identity.AcceptInvitationInput{
				Token: token, Username: "invitee", Password: password,
			}); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatalf("acceptance = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestExpiredAndRevokedSessionsFail(t *testing.T) {
	service, db, _ := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for _, test := range []struct {
		name   string
		expire bool
		revoke bool
	}{
		{name: "expired", expire: true},
		{name: "revoked", revoke: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			credentials, err := service.Login(ctx, identity.LoginInput{Login: "root", Password: password})
			if err != nil {
				t.Fatalf("Login: %v", err)
			}

			if test.expire {
				if _, err := db.ExecContext(ctx,
					"UPDATE sessions SET expires_at = NOW() - INTERVAL '1 hour' WHERE id = $1", credentials.Session.ID); err != nil {
					t.Fatalf("expire the session: %v", err)
				}
			}
			if test.revoke {
				if err := service.Logout(ctx, credentials.SessionToken); err != nil {
					t.Fatalf("Logout: %v", err)
				}
			}

			if _, err := service.CurrentUser(ctx, credentials.SessionToken); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatalf("CurrentUser = %v, want ErrInvalidToken", err)
			}
		})
	}
}

// TestDisablingAnAccountRevokesItsSessionsAndRefusesSignIn covers the whole
// account-disable path through the real store.
func TestDisablingAnAccountRevokesItsSessionsAndRefusesSignIn(t *testing.T) {
	service, db, admin := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	member := inviteAndAccept(t, service, admin.ID, "member@example.com", "member")
	credentials, err := service.Login(ctx, identity.LoginInput{Login: "member", Password: password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, err := db.ExecContext(ctx, "UPDATE users SET status = 'disabled' WHERE id = $1", member.ID); err != nil {
		t.Fatalf("disable the account: %v", err)
	}
	if err := service.RevokeAllSessions(ctx, member.ID); err != nil {
		t.Fatalf("RevokeAllSessions: %v", err)
	}

	if _, err := service.CurrentUser(ctx, credentials.SessionToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("CurrentUser after disabling = %v, want ErrInvalidToken", err)
	}
	if _, err := service.Login(ctx, identity.LoginInput{Login: "member", Password: password}); !errors.Is(err, identity.ErrAccountDisabled) {
		t.Fatalf("Login after disabling = %v, want ErrAccountDisabled", err)
	}
}

// TestLoginThroughPostgres covers the sign-in paths against the real queries.
func TestLoginThroughPostgres(t *testing.T) {
	service, _, _ := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for _, login := range []string{"root", "ROOT", "root@example.com", " Root@Example.COM "} {
		if _, err := service.Login(ctx, identity.LoginInput{Login: login, Password: password}); err != nil {
			t.Errorf("Login(%q): %v", login, err)
		}
	}
	if _, err := service.Login(ctx, identity.LoginInput{Login: "root", Password: "wrong password"}); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("wrong password = %v, want ErrInvalidCredentials", err)
	}
	if _, err := service.Login(ctx, identity.LoginInput{Login: "ghost", Password: password}); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("unknown account = %v, want ErrInvalidCredentials", err)
	}
}

// TestInvitationRequiresAnActiveAdministrator covers the permission check
// against the real store.
func TestInvitationRequiresAnActiveAdministrator(t *testing.T) {
	service, _, admin := integrationService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	member := inviteAndAccept(t, service, admin.ID, "member@example.com", "member")

	if _, _, err := service.CreateInvitation(ctx, identity.CreateInvitationInput{
		ActorID: member.ID, Email: "someone@example.com",
	}); !errors.Is(err, identity.ErrNotAdmin) {
		t.Fatalf("a member created an invitation: %v", err)
	}
}
