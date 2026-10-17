package identity

import (
	"context"
	"errors"
	"testing"
)

// TestRotateCSRFTokenIssuesAUsableToken covers the property the server-rendered
// surfaces depend on: a document can be handed a token, and that token verifies
// against the session it belongs to.
//
// It exists because only the token's digest is stored, so the value behind it
// cannot be recovered — a page load has to be issued a new one, and that issue is
// what makes a reload able to submit a form.
func TestRotateCSRFTokenIssuesAUsableToken(t *testing.T) {
	service, _, _, member := seeded(t)
	ctx := context.Background()

	credentials, err := service.Login(ctx, LoginInput{Login: member.Username, Password: testPassword})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	issued, err := service.RotateCSRFToken(ctx, credentials.SessionToken)
	if err != nil {
		t.Fatalf("RotateCSRFToken: %v", err)
	}
	if issued == "" {
		t.Fatal("the rotation issued an empty token")
	}
	// A document receives a genuinely fresh value rather than the sign-in one.
	if issued == credentials.CSRFToken {
		t.Error("the rotation returned the sign-in token unchanged")
	}

	// The session now carries the issued token's digest, and the superseded token
	// no longer verifies: at most one token is live per session.
	_, _, digest, err := service.ResolveSession(ctx, credentials.SessionToken)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if !DigestsMatch(HashToken(issued), digest) {
		t.Error("the session does not carry the issued token's digest")
	}
	if DigestsMatch(HashToken(credentials.CSRFToken), digest) {
		t.Error("the superseded token still verifies")
	}
}

// TestRotateCSRFTokenRefusesWhatItShould covers the denied paths.
func TestRotateCSRFTokenRefusesWhatItShould(t *testing.T) {
	service, store, _, member := seeded(t)
	ctx := context.Background()

	credentials, err := service.Login(ctx, LoginInput{Login: member.Username, Password: testPassword})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "no token", token: ""},
		{name: "an unknown token", token: "not-a-session"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.RotateCSRFToken(ctx, test.token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("error = %v, want ErrInvalidToken", err)
			}
		})
	}

	// A revoked session cannot be issued a token. This is the rule that matters:
	// otherwise revoking a session would leave a usable token behind.
	if err := service.Logout(ctx, credentials.SessionToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := service.RotateCSRFToken(ctx, credentials.SessionToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a revoked session = %v, want ErrInvalidToken", err)
	}

	_ = store
}

// TestRotateCSRFTokenStoresOnlyTheDigest is the storage rule from
// docs/architecture/secrets.md: a high-entropy token is kept as a SHA-256 digest,
// so a database leak yields nothing to present to the API.
func TestRotateCSRFTokenStoresOnlyTheDigest(t *testing.T) {
	service, store, _, member := seeded(t)
	ctx := context.Background()

	credentials, err := service.Login(ctx, LoginInput{Login: member.Username, Password: testPassword})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	issued, err := service.RotateCSRFToken(ctx, credentials.SessionToken)
	if err != nil {
		t.Fatalf("RotateCSRFToken: %v", err)
	}

	// The fake store is read directly rather than through a helper, because the
	// point is what the store actually holds.
	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.sessions) == 0 {
		t.Fatal("no session was stored")
	}
	for _, record := range store.sessions {
		if string(record.csrfHash) == issued {
			t.Fatal("the store holds the CSRF token in the clear")
		}
		if len(record.csrfHash) != 32 {
			t.Errorf("the stored digest is %d bytes, want 32 (SHA-256)", len(record.csrfHash))
		}
		// And it is the digest of the issued token, so the rotation landed.
		if !DigestsMatch(HashToken(issued), record.csrfHash) {
			t.Error("the stored digest is not the issued token's")
		}
	}
}
