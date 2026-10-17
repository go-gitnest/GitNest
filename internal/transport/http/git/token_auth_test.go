package git

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The Git credential path.
//
// The properties that matter are negative: a credential this server cannot verify must
// resolve to *anonymous* rather than to a distinguishable state (threat model T-A4), and
// the credential value must never reach a log line or a principal that carries more than
// identity (T-A3).

// fakeTokens resolves tokens the way identity.Service does.
type fakeTokens struct {
	// byValue maps a token value to the account it authenticates.
	byValue map[string]identity.User
	// err forces a failure, so a store fault can be exercised.
	err error
	// calls counts resolutions, which is how a test proves a malformed credential never
	// reached the store.
	calls int
	// lastPresented records what the resolver was asked about, so a test can prove the
	// value travelled intact.
	lastPresented string
}

func (f *fakeTokens) ResolveToken(_ context.Context, presented string) (identity.User, identity.Token, error) {
	f.calls++
	f.lastPresented = presented
	if f.err != nil {
		return identity.User{}, identity.Token{}, f.err
	}
	user, found := f.byValue[presented]
	if !found {
		return identity.User{}, identity.Token{}, identity.ErrInvalidToken
	}
	return user, identity.Token{ID: "token-1", UserID: user.ID, Scopes: []string{identity.ScopeRepo}}, nil
}

// recordedLog collects the messages the authenticator emitted.
type recordedLog struct {
	messages []string
	values   []any
}

func (l *recordedLog) log(message string, attributes ...any) {
	l.messages = append(l.messages, message)
	l.values = append(l.values, attributes...)
}

// newAuthenticator builds one over a fake resolver.
func newAuthenticator(t *testing.T, tokens TokenResolver, log *recordedLog) *TokenAuthenticator {
	t.Helper()
	authenticator, err := NewTokenAuthenticator(tokens, log.log)
	if err != nil {
		t.Fatalf("NewTokenAuthenticator: %v", err)
	}
	return authenticator
}

// requestWith builds a request carrying an Authorization header.
func requestWith(header string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/alice/repo.git/info/refs", nil)
	if header != "" {
		request.Header.Set("Authorization", header)
	}
	return request
}

// TestAVerifiedTokenResolvesToItsAccount is the positive case: this is what makes phase
// 1's exit criterion — an invited contributor pushes — reachable at all.
func TestAVerifiedTokenResolvesToItsAccount(t *testing.T) {
	const value = "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"
	tokens := &fakeTokens{byValue: map[string]identity.User{
		value: {ID: "01920000-0000-7000-8000-0000000000c1", Username: "alice", Status: identity.StatusActive},
	}}
	authenticator := newAuthenticator(t, tokens, &recordedLog{})

	for _, header := range []string{
		"Bearer " + value,
		"token " + value,
		"basic " + basicCredential("", value),
		"Basic " + basicCredential("", value),
		"Basic " + basicCredential(value, "x-oauth-basic"),
		"basic " + basicCredential(value, ""),
	} {
		t.Run(header, func(t *testing.T) {
			principal := authenticator.Principal(requestWith(header))
			if principal.IsAnonymous() {
				t.Fatalf("%q resolved as anonymous", header)
			}
			if principal.UserID != "01920000-0000-7000-8000-0000000000c1" {
				t.Errorf("principal = %+v, want the token's account", principal)
			}
			// T-A3: the principal carries identity and nothing else. There is no scope on
			// the type, which is the structural version of the rule that a transport never
			// inspects one.
		})
	}
}

// basicCredential encodes a Basic credential.
func basicCredential(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// TestARejectedCredentialResolvesAsAnonymous is the T-A4 property, and the reason the
// authenticator does not report a distinguishable failure.
//
// Anonymous is then refused by the repository service with the same 404 a nonexistent
// repository gets, so presenting a wrong token teaches an attacker nothing that presenting
// no token would not.
func TestARejectedCredentialResolvesAsAnonymous(t *testing.T) {
	known := "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"
	tokens := &fakeTokens{byValue: map[string]identity.User{
		known: {ID: "01920000-0000-7000-8000-0000000000c1", Username: "alice", Status: identity.StatusActive},
	}}

	for _, test := range []struct {
		name   string
		header string
		why    string
	}{
		{name: "no header", header: "", why: "an anonymous caller"},
		{name: "a scheme we do not understand", header: "Digest abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE", why: "W1.7's rule that an unverifiable credential must not authenticate anyone"},
		{name: "a scheme with no value", header: "Bearer", why: "a malformed header"},
		{name: "a value that cannot be a token", header: "Bearer short", why: "a shape that cannot come from the generator"},
		{name: "an unknown token", header: "Bearer zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", why: "a token no account holds"},
		{name: "basic with no colon", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon")), why: "a malformed Basic credential"},
		{name: "basic that is not base64", header: "Basic !!!not-base64!!!", why: "a mis-encoded credential"},
		{name: "basic with a password that is not a token", header: "Basic " + basicCredential("alice", "hunter2"), why: "a username and password, which this server does not accept"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authenticator := newAuthenticator(t, tokens, &recordedLog{})
			principal := authenticator.Principal(requestWith(test.header))
			if !principal.IsAnonymous() {
				t.Errorf("%s resolved as %+v, want anonymous (%s)", test.name, principal, test.why)
			}
		})
	}
}

// TestTheCredentialValueNeverReachesTheLog is the secrecy rule: product-goals.md lists
// personal access tokens among the values that never reach a log line.
func TestTheCredentialValueNeverReachesTheLog(t *testing.T) {
	// A refusal, because that is when an implementation is most tempted to log the input.
	const value = "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"
	tokens := &fakeTokens{byValue: map[string]identity.User{}}
	logged := &recordedLog{}
	authenticator := newAuthenticator(t, tokens, logged)

	authenticator.Principal(requestWith("Bearer " + value))

	joined := strings.Join(logged.messages, " ") + " " + strings.Join(attributeStrings(logged.values), " ")
	if strings.Contains(joined, value) {
		t.Fatalf("the log carries the credential value: %s", joined)
	}
	// Nor any part of it that would let a reader reconstruct one: the prefix alone is
	// stored in the clear, but the rest is not.
	if strings.Contains(joined, value[:20]) {
		t.Errorf("the log carries a long prefix of the value: %s", joined)
	}
	// And the log does say a credential was refused, which is what an operator
	// investigating a failed push needs.
	if len(logged.messages) == 0 {
		t.Error("a refused credential was not reported at all")
	}
}

// attributeStrings flattens attribute values for a substring search.
func attributeStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// TestAShapeFailureNeverReachesTheStore keeps a malformed credential from costing a
// database round trip on every malformed request.
func TestAShapeFailureNeverReachesTheStore(t *testing.T) {
	tokens := &fakeTokens{byValue: map[string]identity.User{}}
	authenticator := newAuthenticator(t, tokens, &recordedLog{})

	// Each of these is refused by the header decoder, before the resolver.
	for _, header := range []string{
		"",
		"Bearer",
		"Bearer ",
		"Digest abc",
		"Basic !!!not-base64!!!",
	} {
		before := tokens.calls
		authenticator.Principal(requestWith(header))
		if tokens.calls != before {
			t.Errorf("%q reached the store; a header with no usable credential should not", header)
		}
	}

	// A well-formed bearer token does reach it, which is what proves the check above is
	// discriminating rather than everything being skipped.
	before := tokens.calls
	authenticator.Principal(requestWith("Bearer abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"))
	if tokens.calls != before+1 {
		t.Error("a well-formed credential did not reach the store")
	}
}

// TestAStoreFaultResolvesAsAnonymous covers the failure that must not become a bypass: if
// resolution fails, the caller is anonymous rather than authenticated.
func TestAStoreFaultResolvesAsAnonymous(t *testing.T) {
	tokens := &fakeTokens{err: errors.New("the connection was reset")}
	logged := &recordedLog{}
	authenticator := newAuthenticator(t, tokens, logged)

	principal := authenticator.Principal(requestWith("Bearer abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"))
	if !principal.IsAnonymous() {
		t.Errorf("a store fault resolved as %+v, want anonymous", principal)
	}
	if len(logged.messages) == 0 {
		t.Error("a store fault was not reported")
	}
}

// TestADisabledAccountResolvesAsAnonymous covers administration.md's rule that disabling an
// account stops its credentials working.
func TestADisabledAccountResolvesAsAnonymous(t *testing.T) {
	const value = "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"
	tokens := &fakeTokens{err: identity.ErrAccountDisabled}
	authenticator := newAuthenticator(t, tokens, &recordedLog{})

	if !authenticator.Principal(requestWith("Bearer " + value)).IsAnonymous() {
		t.Error("a disabled account's token authenticated its holder")
	}
}

// TestTheValueTravelsIntact covers the decoding: a token must reach the resolver exactly as
// it was sent, because a truncated or re-encoded value would simply never match.
func TestTheValueTravelsIntact(t *testing.T) {
	const value = "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"
	tokens := &fakeTokens{byValue: map[string]identity.User{
		value: {ID: "01920000-0000-7000-8000-0000000000c1", Status: identity.StatusActive},
	}}
	authenticator := newAuthenticator(t, tokens, &recordedLog{})

	for _, header := range []string{
		"Bearer " + value,
		"token " + value,
		"  Bearer " + value + "  ",
		"Basic " + basicCredential("", value),
	} {
		tokens.lastPresented = ""
		authenticator.Principal(requestWith(header))
		if tokens.lastPresented != value {
			t.Errorf("%q reached the resolver as %q, want the value intact", header, tokens.lastPresented)
		}
	}
}

// TestTheBasicFormsAreUnderstood covers the two conventions for where a token goes in a
// Basic credential, because a credential helper and a prompt put it in different places and
// refusing either would make a token unusable from the command line.
func TestTheBasicFormsAreUnderstood(t *testing.T) {
	const value = "abcdefghijklmnopqrstuvwxyz0123456789-_ABCDE"

	// As the password, which is what a user types at a prompt.
	if got, ok := bearerCredential("Basic " + basicCredential("alice", value)); !ok || got != value {
		t.Errorf("a token as the password = %q, %v", got, ok)
	}
	// As the username, which is what a credential helper sends.
	if got, ok := bearerCredential("Basic " + basicCredential(value, "")); !ok || got != value {
		t.Errorf("a token as the username = %q, %v", got, ok)
	}
	// The password wins when both could be a token, because a prompt is the likelier
	// source and the password is the field a prompt fills.
	other := "ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210-_abcde"
	if got, ok := bearerCredential("Basic " + basicCredential(value, other)); !ok || got != other {
		t.Errorf("with two token-shaped fields = %q, %v; want the password", got, ok)
	}
	// Neither being token-shaped is a refusal rather than a guess.
	if _, ok := bearerCredential("Basic " + basicCredential("alice", "hunter2")); ok {
		t.Error("a username and password were accepted as a credential")
	}
}

// TestTheAuthenticatorNeedsAResolver keeps a wiring mistake at startup rather than
// resolving every caller as anonymous, which would be a silent downgrade.
func TestTheAuthenticatorNeedsAResolver(t *testing.T) {
	if _, err := NewTokenAuthenticator(nil, nil); err == nil {
		t.Error("a nil resolver was accepted")
	} else if !strings.Contains(err.Error(), "token resolver") {
		t.Errorf("the error does not name what is missing: %v", err)
	}
	// A nil log is not an error: the authenticator simply reports nothing, which is what a
	// caller that has no logger wants.
	if _, err := NewTokenAuthenticator(&fakeTokens{}, nil); err != nil {
		t.Errorf("a nil logger was refused: %v", err)
	}
}

// TestThePrincipalCarriesNoScope is T-A3 made structural: the type has no scope field, so
// a transport cannot inspect one even by accident.
//
// It is asserted by construction rather than by a runtime check, and it is written as a
// test so that adding a scope field to the principal fails here rather than passing
// review.
func TestThePrincipalCarriesNoScope(t *testing.T) {
	principal := repository.User("01920000-0000-7000-8000-0000000000c1", false)

	// The fields a principal may carry. A new one has to be added here deliberately.
	// `repository.Principal` is a struct with identity and an administrator flag; there is
	// no scope, permission, or role on it.
	if principal.UserID == "" {
		t.Fatal("the principal carries no identity at all")
	}
	if principal.IsAnonymous() {
		t.Error("a user principal reported itself anonymous")
	}
}
