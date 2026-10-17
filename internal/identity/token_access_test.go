package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The personal access token rules.
//
// The load-bearing properties are negative ones, which is why most of these tests assert
// a refusal: the plaintext never comes back, a revoked token stops working immediately,
// another account cannot revoke it, and an ambiguous prefix is refused rather than
// guessed at.

// tokenFixture returns a service with one account.
func tokenFixture(t *testing.T) (*Service, *fakeStore, User) {
	t.Helper()
	service, store, _, member := seeded(t)
	return service, store, member
}

// TestCreateTokenReturnsTheValueOnce is the property administration.md and W4.8 both
// require: "shown once at creation", and the plaintext is "never persisted, returned after
// creation, or present in any log line".
func TestCreateTokenReturnsTheValueOnce(t *testing.T) {
	service, store, member := tokenFixture(t)
	ctx := context.Background()

	created, err := service.CreateToken(ctx, CreateTokenInput{
		ActorID: member.ID, Name: "laptop",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if created.Value == "" {
		t.Fatal("no value was returned")
	}
	if len(created.Value) != MinTokenLength {
		t.Errorf("the value is %d characters, want %d", len(created.Value), MinTokenLength)
	}
	// The default scope is `repo`, because a token with no scope would authenticate a
	// caller and grant it nothing.
	if !created.Token.HasScope(ScopeRepo) {
		t.Errorf("scopes = %v, want the default repo", created.Token.Scopes)
	}
	// The prefix is the leading characters, and it is not the whole value.
	if created.Token.Prefix != PrefixOf(created.Value) {
		t.Errorf("prefix = %q, want the leading characters of the value", created.Token.Prefix)
	}
	if created.Token.Prefix == created.Value {
		t.Error("the stored prefix is the whole token")
	}

	// **The store holds the digest and never the value.** This is the assertion that
	// matters: a leak here would be a leak in every listing and every backup.
	//
	// The lock is taken and released in a scope of its own rather than held by a defer for
	// the rest of the test: `ResolveToken` below takes the same lock through the store, and
	// holding it here would deadlock the test rather than failing it — which is exactly
	// what happened when this was first written.
	inspectStoredDigest(t, store, created.Value)

	// A second call cannot produce the value again, because nothing can.
	resolved, _, err := service.ResolveToken(ctx, created.Value)
	if err != nil {
		t.Fatalf("the token it just issued does not authenticate: %v", err)
	}
	if resolved.ID != member.ID {
		t.Errorf("the token authenticated %q, want the issuing account", resolved.Username)
	}
}

// TestTheValueIsNotRecoverableFromTheListing is the other half of "shown once".
func TestTheValueIsNotRecoverableFromTheListing(t *testing.T) {
	service, _, member := tokenFixture(t)
	ctx := context.Background()

	created, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "ci"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	listed, err := service.ListTokens(ctx, member.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("the listing has %d tokens, want 1", len(listed))
	}
	for _, token := range listed {
		if token.Prefix == created.Value {
			t.Error("the listing carries the whole value")
		}
		// The type has no field for the digest or the value at all, which is the
		// structural version of this guarantee.
	}
}

// TestARevokedTokenStopsWorkingImmediately is secrets.md's rule: "a revoked session,
// token, or key fails on the next request, and any cache that could answer otherwise is
// invalidated on revocation".
func TestARevokedTokenStopsWorkingImmediately(t *testing.T) {
	service, _, member := tokenFixture(t)
	ctx := context.Background()

	created, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "revocable"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if _, _, err := service.ResolveToken(ctx, created.Value); err != nil {
		t.Fatalf("the token does not work before revocation: %v", err)
	}

	revoked, err := service.RevokeToken(ctx, member.ID, created.Token.ID)
	if err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if !revoked {
		t.Fatal("the revocation reported that nothing was revoked")
	}

	// The very next resolution fails, and fails as an invalid token rather than as
	// something a caller could use to tell a revoked token from an unknown one.
	if _, _, err := service.ResolveToken(ctx, created.Value); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a revoked token = %v, want ErrInvalidToken", err)
	}

	// Revoking again is not an error, and reports that nothing changed.
	again, err := service.RevokeToken(ctx, member.ID, created.Token.ID)
	if err != nil {
		t.Errorf("revoking an already-revoked token: %v", err)
	}
	if again {
		t.Error("the second revocation claimed to revoke something")
	}
	// And it is still listed, so its owner sees that it was revoked rather than wondering
	// where it went.
	listed, err := service.ListTokens(ctx, member.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(listed) != 1 || !listed[0].Revoked() {
		t.Errorf("the listing does not show the token as revoked: %+v", listed)
	}
}

// TestAnotherAccountCannotRevokeAToken is the authorization rule, asserted rather than
// assumed: the scoping is in the statement, so a caller that learns an identifier still
// cannot act on a token it does not own.
func TestAnotherAccountCannotRevokeAToken(t *testing.T) {
	service, _, member := tokenFixture(t)
	ctx := context.Background()

	created, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "mine"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// The admin account is a different account with the identifier that exists.
	revoked, err := service.RevokeToken(ctx, adminUserID(t, service), created.Token.ID)
	if err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if revoked {
		t.Fatal("another account revoked a token it does not own")
	}
	// The token still works.
	if _, _, err := service.ResolveToken(ctx, created.Value); err != nil {
		t.Errorf("the token stopped working though it was not revoked: %v", err)
	}
}

// inspectStoredDigest asserts that the store holds the value's digest and not the value.
func inspectStoredDigest(t *testing.T, store *fakeStore, value string) {
	t.Helper()

	store.mu.Lock()
	defer store.mu.Unlock()

	for _, stored := range store.tokens {
		if string(stored.tokenHash) == value {
			t.Fatal("the store holds the token in the clear")
		}
		if len(stored.tokenHash) != 32 {
			t.Errorf("the stored digest is %d bytes, want 32 (SHA-256)", len(stored.tokenHash))
		}
		if !DigestsMatch(stored.tokenHash, HashToken(value)) {
			t.Error("the stored digest is not the value's")
		}
	}
}

// adminUserID returns the seeded administrator's identifier.
func adminUserID(t *testing.T, service *Service) string {
	t.Helper()
	// The fixture seeds an administrator named `root`; the identifier is not known here,
	// so it is read from the store the service was built over.
	store := service.store.(*fakeStore)
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, account := range store.users {
		if account.user.IsAdmin {
			return account.user.ID
		}
	}
	t.Fatal("the fixture has no administrator")
	return ""
}

// TestADisabledAccountTokenStopsWorking covers administration.md's rule that disabling an
// account "does not restore revoked sessions, tokens, or keys" — which is only true if a
// disabled account is refused when its token is presented.
func TestADisabledAccountTokenStopsWorking(t *testing.T) {
	service, store, member := tokenFixture(t)
	ctx := context.Background()

	created, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "disabled-later"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	store.mu.Lock()
	for _, account := range store.users {
		if account.user.ID == member.ID {
			account.user.Status = StatusDisabled
		}
	}
	store.mu.Unlock()

	if _, _, err := service.ResolveToken(ctx, created.Value); !errors.Is(err, ErrAccountDisabled) {
		t.Errorf("a disabled account's token = %v, want ErrAccountDisabled", err)
	}
	// And a new token cannot be issued for it either.
	if _, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "nope"}); !errors.Is(err, ErrAccountDisabled) {
		t.Errorf("creating a token for a disabled account = %v, want ErrAccountDisabled", err)
	}
}

// TestAMalformedTokenIsRefusedWithoutALookup covers the shape check: a value that cannot
// have come from the generator is refused before a database round trip, and refused with
// the same error an unknown token produces.
func TestAMalformedTokenIsRefusedWithoutALookup(t *testing.T) {
	service, _, _ := tokenFixture(t)
	ctx := context.Background()

	for _, value := range []string{
		"",
		"   ",
		"short",
		strings.Repeat("x", MinTokenLength+1),
		strings.Repeat("x", MinTokenLength-1),
		// A copy-paste artefact: the token with a trailing newline becomes two values in a
		// header, and the space here stands in for that.
		strings.Repeat("x", MinTokenLength-1) + " ",
		// A character base64url cannot produce.
		strings.Repeat("x", MinTokenLength-1) + "+",
		"@/" + strings.Repeat("x", MinTokenLength-2),
	} {
		t.Run(value, func(t *testing.T) {
			if _, _, err := service.ResolveToken(ctx, value); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("ResolveToken(%q) = %v, want ErrInvalidToken", value, err)
			}
		})
	}
}

// TestAnUnknownTokenIsRefusedIndistinguishablyFromARevokedOne keeps the refusal from
// becoming an oracle: a caller must not learn from the answer whether a token exists.
func TestAnUnknownTokenIsRefusedIndistinguishablyFromARevokedOne(t *testing.T) {
	service, _, member := tokenFixture(t)
	ctx := context.Background()

	created, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "known"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if _, err := service.RevokeToken(ctx, member.ID, created.Token.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	// A token that was issued and revoked.
	_, _, revokedErr := service.ResolveToken(ctx, created.Value)
	// A token that was never issued, of the same shape.
	never, digest, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	_ = digest
	_, _, unknownErr := service.ResolveToken(ctx, never)

	if !errors.Is(revokedErr, ErrInvalidToken) || !errors.Is(unknownErr, ErrInvalidToken) {
		t.Fatalf("revoked = %v, unknown = %v; both must be ErrInvalidToken", revokedErr, unknownErr)
	}
	// The messages are identical, so nothing in the response distinguishes them.
	if revokedErr.Error() != unknownErr.Error() {
		t.Errorf("the refusals differ: %q and %q", revokedErr, unknownErr)
	}
}

// TestScopesAreValidatedRatherThanDropped covers the rule that an unknown scope is
// refused: dropping it would issue a token with fewer permissions than the caller asked
// for and no indication the request was partly ignored.
func TestScopesAreValidatedRatherThanDropped(t *testing.T) {
	service, _, member := tokenFixture(t)
	ctx := context.Background()

	for _, test := range []struct {
		name    string
		scopes  []string
		wantErr bool
	}{
		{name: "no scopes defaults to repo", scopes: nil},
		{name: "repo", scopes: []string{ScopeRepo}},
		{name: "repo twice is one scope", scopes: []string{ScopeRepo, ScopeRepo}},
		{name: "an unknown scope", scopes: []string{"admin:enterprise"}, wantErr: true},
		{name: "a phase-4 scope this build does not enforce", scopes: []string{"workflow"}, wantErr: true},
		{name: "a mixture", scopes: []string{ScopeRepo, "packages"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			created, err := service.CreateToken(ctx, CreateTokenInput{
				ActorID: member.ID, Name: "scoped", Scopes: test.scopes,
			})
			if test.wantErr {
				if err == nil {
					t.Fatal("an unrecognised scope was accepted")
				}
				// The refusal names the accepted values, so a client can correct itself.
				if !strings.Contains(err.Error(), ScopeRepo) {
					t.Errorf("the refusal does not name the accepted scopes: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateToken: %v", err)
			}
			if len(created.Token.Scopes) != 1 || created.Token.Scopes[0] != ScopeRepo {
				t.Errorf("scopes = %v, want exactly one repo", created.Token.Scopes)
			}
		})
	}
}

// TestTokenNamesAreValidated covers the one field a person types.
func TestTokenNamesAreValidated(t *testing.T) {
	service, _, member := tokenFixture(t)
	ctx := context.Background()

	for _, name := range []string{"", "   ", strings.Repeat("x", MaxTokenNameLength+1)} {
		if _, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: name}); err == nil {
			t.Errorf("the name %q was accepted", name)
		}
	}
	// A name is trimmed, so a stray space does not become part of it.
	created, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "  padded  "})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if created.Token.Name != "padded" {
		t.Errorf("the stored name is %q, want it trimmed", created.Token.Name)
	}
}

// TestRevokeByPrefixRefusesAnAmbiguousPrefix is the administrator path: a prefix is
// twelve characters and two tokens issued seconds apart can share one, so an ambiguous
// match is reported rather than guessed at.
func TestRevokeByPrefixRefusesAnAmbiguousPrefix(t *testing.T) {
	service, store, member := tokenFixture(t)
	ctx := context.Background()

	// Two tokens whose prefixes are made identical, which the generator will not do but a
	// collision can.
	first, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "first"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	second, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "second"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	shared := first.Token.Prefix
	store.mu.Lock()
	for _, stored := range store.tokens {
		if stored.token.ID == second.Token.ID {
			stored.token.Prefix = shared
		}
	}
	store.mu.Unlock()

	revoked, err := service.RevokeTokensByPrefix(ctx, shared)
	if err == nil {
		t.Fatal("an ambiguous prefix was accepted")
	}
	if !strings.Contains(err.Error(), "share the prefix") {
		t.Errorf("the refusal does not explain the ambiguity: %v", err)
	}
	// Both were revoked by the statement, and the count says so rather than reporting a
	// silent partial success.
	if revoked != 2 {
		t.Errorf("revoked %d, want the 2 that matched", revoked)
	}

	// A prefix that matches one token revokes it and reports one.
	third, err := service.CreateToken(ctx, CreateTokenInput{ActorID: member.ID, Name: "unique"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	revoked, err = service.RevokeTokensByPrefix(ctx, third.Token.Prefix)
	if err != nil {
		t.Fatalf("RevokeTokensByPrefix: %v", err)
	}
	if revoked != 1 {
		t.Errorf("revoked %d, want 1", revoked)
	}
	// And the token it named no longer works.
	if _, _, err := service.ResolveToken(ctx, third.Value); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("the revoked token still works: %v", err)
	}
}

// TestATooShortPrefixIsRefused keeps a one-character prefix from matching every token on
// the instance.
func TestATooShortPrefixIsRefused(t *testing.T) {
	service, _, _ := tokenFixture(t)

	for _, prefix := range []string{"", "   ", "a", "abc", "abcdefg"} {
		if _, err := service.RevokeTokensByPrefix(context.Background(), prefix); err == nil {
			t.Errorf("the prefix %q was accepted", prefix)
		}
	}
}

// TestRevokingAnUnknownPrefixReportsZero keeps the administrator command honest: a prefix
// that matches nothing reports nothing, rather than an error that reads like a failure.
func TestRevokingAnUnknownPrefixReportsZero(t *testing.T) {
	service, _, _ := tokenFixture(t)

	revoked, err := service.RevokeTokensByPrefix(context.Background(), "nosuchprefix")
	if err != nil {
		t.Fatalf("RevokeTokensByPrefix: %v", err)
	}
	if revoked != 0 {
		t.Errorf("revoked %d, want 0", revoked)
	}
}

// TestPrefixOfIsStable keeps the administrator path and the stored value in agreement.
func TestPrefixOfIsStable(t *testing.T) {
	if got := PrefixOf("abcdefghijklmnop"); got != "abcdefghijkl" {
		t.Errorf("PrefixOf = %q", got)
	}
	// A value shorter than the prefix is returned whole rather than padded.
	if got := PrefixOf("abc"); got != "abc" {
		t.Errorf("PrefixOf(short) = %q", got)
	}
	if got := PrefixOf(""); got != "" {
		t.Errorf("PrefixOf(empty) = %q", got)
	}
}
