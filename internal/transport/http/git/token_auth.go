package git

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The token authenticator: the implementation the Authenticator seam was built for.
//
// `docs/architecture/urls-and-identifiers.md` names the accepted credentials for Git over
// HTTP — "Basic with a personal access token, OAuth/app token, or an SSH key" — and
// `docs/roadmap/04-api-and-extensibility.md` W4.8 names the header forms for the API:
// "Authentication accepts `Authorization: token <t>` and `Authorization: Bearer <t>`".
//
// Two rules from the threat model decide the behaviour below:
//
//   - **T-A3: a transport never inspects a scope.** This type answers exactly one
//     question — "who is this?" — and returns a `repository.Principal` that carries
//     identity and nothing else. Whether that identity may read or write is decided by
//     `internal/repository`, which is the same decision the API and the browser get.
//   - **T-A4: a refusal must not disclose existence.** A credential this server cannot
//     verify resolves to *anonymous*, not to a distinguishable "bad credential" state.
//     An anonymous caller is then refused by the repository service with the same 404 a
//     nonexistent repository gets, so presenting a wrong token teaches an attacker
//     nothing that presenting no token would not. W1.7 established this shape and it is
//     preserved rather than reinterpreted.
//
// The consequence is deliberate and worth stating: a caller with a *valid* token that
// lacks the required level is refused as not-found, and a caller with an *invalid* token
// is refused identically. A client that wants to know whether its credential is good
// should call the API, where the answer is not an existence oracle.

// TokenResolver turns a presented token into the account it authenticates.
//
// It is an interface rather than a concrete `*identity.Service` so the transport stays
// testable without a database, exactly as `AccessService` is. It is satisfied by
// `identity.Service`.
type TokenResolver interface {
	// ResolveToken returns the account and token a credential authenticates. A token that
	// is malformed, unknown, or revoked reports an error, and the caller treats every one
	// of those as anonymous.
	ResolveToken(ctx context.Context, presented string) (identity.User, identity.Token, error)
}

// TokenAuthenticator resolves a Git request's `Authorization` header.
type TokenAuthenticator struct {
	tokens TokenResolver
	// log receives the notice of a rejected credential. It must not receive the
	// credential: `product-goals.md` lists personal access tokens among the values that
	// never reach a log line, so the message names the outcome and not the value.
	log func(message string, attributes ...any)
}

// NewTokenAuthenticator returns an authenticator over a token resolver.
func NewTokenAuthenticator(tokens TokenResolver, log func(string, ...any)) (*TokenAuthenticator, error) {
	if tokens == nil {
		return nil, ErrNoTokenResolver
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	return &TokenAuthenticator{tokens: tokens, log: log}, nil
}

// ErrNoTokenResolver reports a missing dependency, so a wiring mistake fails at startup
// rather than resolving every caller as anonymous.
var ErrNoTokenResolver = errors.New("the Git transport's token authenticator needs a token resolver")

// Principal resolves the caller from the request's credential.
//
// Every failure resolves to anonymous rather than to an error the caller could observe,
// for the T-A4 reason recorded above. The request context is used so a cancelled request
// does not leave a database call running.
func (a *TokenAuthenticator) Principal(r *http.Request) repository.Principal {
	presented, ok := bearerCredential(r.Header.Get("Authorization"))
	if !ok {
		return repository.Anonymous()
	}

	user, _, err := a.tokens.ResolveToken(r.Context(), presented)
	if err != nil {
		// The credential is not usable. **The value is never logged** — only the fact that
		// a presented credential was refused, which is what an operator investigating a
		// failed push needs, and which discloses nothing about the token itself.
		a.log("git transport: the presented credential was refused",
			"reason", credentialReason(err))
		return repository.Anonymous()
	}
	return repository.User(user.ID, user.IsAdmin)
}

// credentialReason classifies a refusal without echoing anything derived from the token.
//
// A caller reading a log wants to know whether the token was expired, revoked, or simply
// wrong; the classification names the category and never the digest, the prefix, or the
// value.
func credentialReason(err error) string {
	switch {
	case err == nil:
		return "accepted"
	case errors.Is(err, identity.ErrInvalidToken):
		return "not a live token for any account"
	case errors.Is(err, identity.ErrAccountDisabled):
		return "the account is disabled"
	default:
		return "the credential could not be verified"
	}
}

// bearerCredential extracts a token from an `Authorization` header.
//
// Three forms are accepted, because three are documented or conventional:
//
//   - `Basic` with the token as the username or as the password. Git sends this, and
//     `testing.md` names the shape: "Clone with a personal access token | Credential
//     helper or `http.extraHeader`". A credential helper has no way to send a bearer
//     token, so refusing Basic would make a token unusable from the command line, which
//     is the whole point of issuing one.
//   - `Bearer <token>`, the API form W4.8 names.
//   - `token <token>`, the other API form W4.8 names.
//
// The value is never passed on, logged, or included in an error: it is returned to the
// caller and used once.
func bearerCredential(header string) (string, bool) {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return "", false
	}

	scheme, rest, found := strings.Cut(trimmed, " ")
	rest = strings.TrimSpace(rest)
	if !found || rest == "" {
		return "", false
	}

	switch strings.ToLower(scheme) {
	case "bearer", "token":
		return rest, true
	case "basic":
		decoded, ok := decodeBasic(rest)
		if !ok {
			return "", false
		}
		return decoded, true
	default:
		// An unknown scheme is not a credential this server understands. It is ignored
		// rather than partially honoured, which is W1.7's rule: "a credential this server
		// cannot verify must not appear to authenticate anyone".
		return "", false
	}
}

// decodeBasic reads a token out of a Basic credential.
//
// Git sends `Basic base64(user:password)`, and there are two conventions in the wild for
// where a token goes: as the username with an empty password (a credential helper's
// `username` field), or as the password with a placeholder username (what a user types at
// a prompt). Both are accepted, and the candidate is chosen by which one looks like a
// token — which is a shape check, not a guess: only a string of exactly the right length
// and alphabet can be one this server issued.
//
// The alternative, accepting the username unconditionally, would mean that for every
// request with a username and a password the username is hashed and compared. That is one
// wasted lookup per request and, worse, it would treat a username as a credential.
func decodeBasic(encoded string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Some clients send an unpadded or URL-safe variant. It is attempted rather than
		// refused, because a credential that is present but mis-encoded should still work.
		raw, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return "", false
		}
	}

	user, password, found := strings.Cut(string(raw), ":")
	if !found {
		return "", false
	}

	// The password wins when it could be a token, because that is the shape a user types
	// at a prompt and the more likely intent. Otherwise the username is tried, which is
	// the credential-helper shape.
	if identity.ValidateTokenShape(password) == nil {
		return password, true
	}
	if identity.ValidateTokenShape(user) == nil {
		return user, true
	}
	return "", false
}
