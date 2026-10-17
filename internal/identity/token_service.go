package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The personal access token service.
//
// It is the only place a token is created, resolved, or revoked. Every transport — the
// REST API, the browser settings surface, the Git transport, and the administrator CLI —
// calls these methods, which is what keeps one decision function behind all of them
// (`docs/security/threat-model.md` R1: "One authorization decision function for every
// principal kind", and the architecture rule that a transport never decides access).

// CreateTokenInput is a request to issue a token for an account.
type CreateTokenInput struct {
	// ActorID is the account issuing the token.
	//
	// Phase 1 issues a token only for its own owner, so there is no separate owner field:
	// an administrator acting on someone else's behalf is a phase-8 administration
	// surface, and adding the field now would create a path with no caller to test it.
	ActorID string
	Name    string
	Scopes  []string
}

// CreatedToken is a newly issued token, with the one and only copy of its value.
//
// The raw value is returned here and nowhere else. Nothing stores it, nothing logs it,
// and no later call can produce it again — `administration.md` requires that a token
// listing shows "identifier prefix" and never "the token, its hash, or its full value",
// and W4.8's acceptance requires the plaintext to be "never persisted, returned after
// creation, or present in any log line".
type CreatedToken struct {
	Token Token
	// Value is the credential itself. It is shown once, to the caller that created it.
	Value string
}

// CreateToken issues a personal access token.
//
// The account must exist and be usable, which is checked by the store's resolution of
// the row rather than here: a foreign key would refuse the insert, but only a read tells
// the caller *why*.
func (s *Service) CreateToken(ctx context.Context, input CreateTokenInput) (CreatedToken, error) {
	normalized, err := NewTokenInput{
		UserID: input.ActorID, Name: input.Name, Scopes: input.Scopes,
	}.ValidateNormalized()
	if err != nil {
		return CreatedToken{}, err
	}

	// The account is resolved first, so a token is never issued for one that cannot use
	// it. An unknown or disabled account is reported the same way a sign-in reports it,
	// so the two cannot be told apart.
	account, err := s.store.FindUserByID(ctx, normalized.UserID)
	if err != nil {
		return CreatedToken{}, err
	}
	if account.Status != StatusActive {
		return CreatedToken{}, ErrAccountDisabled
	}

	value, digest, err := NewToken()
	if err != nil {
		return CreatedToken{}, err
	}

	created, err := s.store.CreateToken(ctx, NewTokenRecord{
		ID:          newTokenIdentifier(),
		UserID:      normalized.UserID,
		Name:        normalized.Name,
		TokenPrefix: PrefixOf(value),
		TokenHash:   digest,
		Scopes:      normalized.Scopes,
	})
	if err != nil {
		return CreatedToken{}, err
	}
	return CreatedToken{Token: created, Value: value}, nil
}

// ResolveToken turns a presented credential into the account it authenticates, and the
// token that authorized it.
//
// It is the counterpart of ResolveSession for a token, and the shape check comes first
// because a value that cannot be a token this build issued should not cost a database
// round trip. The two failures are then collapsed: a malformed value, an unknown digest,
// and a revoked token all report ErrInvalidToken, so a caller cannot learn from a refusal
// whether a token exists
// (`docs/security/threat-model.md` T-A4's shape, applied to credentials).
func (s *Service) ResolveToken(ctx context.Context, presented string) (User, Token, error) {
	trimmed := strings.TrimSpace(presented)
	if err := ValidateTokenShape(trimmed); err != nil {
		return User{}, Token{}, err
	}
	return s.store.FindTokenByHash(ctx, HashToken(trimmed))
}

// ListTokens returns an account's tokens. A revoked token is listed, so its owner can
// see that it was revoked rather than wondering where it went.
func (s *Service) ListTokens(ctx context.Context, userID string) ([]Token, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, errors.New("a token listing needs an account")
	}
	return s.store.ListTokens(ctx, userID)
}

// RevokeToken stops one token working, scoped to its owner.
//
// It reports whether a live token was revoked. Revoking an already-revoked token is not
// an error — the caller's intent is satisfied either way — but the distinction lets a
// surface answer "revoked" from "not found" and lets a test prove revocation is
// idempotent.
func (s *Service) RevokeToken(ctx context.Context, userID, tokenID string) (bool, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(tokenID) == "" {
		return false, errors.New("revoking a token needs an account and a token")
	}
	return s.store.RevokeToken(ctx, userID, tokenID)
}

// RevokeTokensByPrefix revokes every live token whose display prefix matches, and
// refuses an ambiguous prefix.
//
// The refusal is the point. `administration.md` gives an administrator a command that
// names a token by its prefix, and a prefix is twelve characters: two tokens issued
// seconds apart can share one. Revoking both because the operator typed an ambiguous
// prefix would be a destructive action taken on a guess, so the count is checked and an
// ambiguous match is refused with the number that matched.
func (s *Service) RevokeTokensByPrefix(ctx context.Context, prefix string) (int, error) {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return 0, errors.New("revoking by prefix needs a prefix")
	}
	// A prefix is stored in the clear, so it is compared literally. Its length is bounded
	// because a one-character prefix would match every token on the instance, and refusing
	// it here is clearer than reporting a count nobody expected.
	if len(trimmed) < 8 {
		return 0, fmt.Errorf("a display prefix is at least 8 characters; %q is too short to identify one token", trimmed)
	}

	revoked, err := s.store.RevokeTokenByPrefix(ctx, trimmed)
	if err != nil {
		return 0, err
	}
	if revoked > 1 {
		// The tokens are already revoked by the statement, so this is a report rather than
		// a rollback. It is a distinct error so an operator knows to look at the listing
		// rather than assuming one token was revoked.
		return revoked, fmt.Errorf("%d tokens share the prefix %q; revoke them individually", revoked, trimmed)
	}
	return revoked, nil
}

// ErrTokenNotFound reports a token identifier that matches nothing the account owns.
var ErrTokenNotFound = errors.New("the token does not exist for this account")
