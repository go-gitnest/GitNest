package identity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Personal access tokens.
//
// The minimum a Git client needs to authenticate as an account, and nothing more.
// `docs/roadmap/04-api-and-extensibility.md` W4.8 owns the full model — fine-grained
// tokens, resource selectors, scope strings beyond `repo`, expiry policy, and the
// last-used instant — and the migration that created this table records why the
// minimum lives in phase 1 instead: the phase-1 **exit criteria** require that an
// invited contributor can push, which is impossible without a credential a Git client
// can present, and no phase-1 workstream delivered one.
//
// What is deliberately **not** here, so a later phase does not have to undo it:
//
//   - **No scope enforcement beyond `repo`.** The database refuses to store a scope the
//     code does not enforce, so a token can never carry a restriction that is not
//     applied. A single `repo` scope means "repository access at the level the account
//     holds", which is the whole of what phase 1 authorizes.
//   - **No expiry.** W4.8 defines the policy. A column that a later phase fills is not a
//     behaviour, and pretending otherwise here would be a security claim nobody tested.
//   - **No last-used update.** The column exists so adding the write is not a migration;
//     the write belongs with the workstream that defines what "used" means for a
//     fine-grained token.

// ScopeRepo is the one scope phase 1 grants.
//
// It is the GitHub classic scope name, because a client that sends one should not have
// to learn a second vocabulary, and because W4.8's model is defined in those terms.
const ScopeRepo = "repo"

// ScopeSet is every scope this build accepts, in the order a message should list them.
//
// It is a list rather than a map so a refusal can name the accepted values in a stable
// order, which is what makes the message testable.
var ScopeSet = []string{ScopeRepo}

// Token length bounds.
//
// A token is 32 random bytes in unpadded base64url, so exactly 43 characters. The bounds
// exist for validation rather than for the generator: a presented value outside them
// cannot be a token this build issued, so it is refused before a database round trip.
const (
	MinTokenLength = 43
	MaxTokenLength = 43
	// tokenPrefixLength is how much of a token is stored in the clear for a listing and
	// for the administrator revoke command.
	//
	// Twelve characters is enough to distinguish two tokens by eye and far too little to
	// be a credential: the remaining thirty-one characters carry the entropy, and the
	// digest is what authenticates. `administration.md` requires a revoke "by its display
	// prefix", and a shorter prefix would collide between two tokens issued seconds
	// apart.
	tokenPrefixLength = 12
	// MaxTokenNameLength bounds the label an owner gives a token.
	MaxTokenNameLength = 100
)

// Token is a personal access token, as anything outside this package sees it.
//
// The digest is absent by design, on the same reasoning as User's password material:
// this type travels to transports and into responses, and a stored digest must not
// travel with it.
type Token struct {
	ID        string
	UserID    string
	Name      string
	Prefix    string
	Scopes    []string
	CreatedAt time.Time
	// LastUsedAt is nil until W4.8 records a use.
	LastUsedAt *time.Time
	// RevokedAt is nil while the token works.
	RevokedAt *time.Time
}

// Revoked reports whether the token no longer authenticates.
func (t Token) Revoked() bool { return t.RevokedAt != nil }

// HasScope reports whether the token carries a scope.
func (t Token) HasScope(scope string) bool {
	for _, granted := range t.Scopes {
		if granted == scope {
			return true
		}
	}
	return false
}

// NewTokenInput is a request to create a token.
type NewTokenInput struct {
	UserID string
	Name   string
	Scopes []string
}

// ValidateNormalized checks a creation request and returns it normalized.
//
// Normalization is trimming the name and de-duplicating the scopes, so a request that
// says `["repo", "repo"]` stores one scope rather than a set that compares unequal to an
// identical grant.
func (i NewTokenInput) ValidateNormalized() (NewTokenInput, error) {
	normalized := NewTokenInput{UserID: i.UserID, Name: strings.TrimSpace(i.Name)}

	if normalized.Name == "" {
		return NewTokenInput{}, errors.New("a token needs a name")
	}
	if len(normalized.Name) > MaxTokenNameLength {
		return NewTokenInput{}, fmt.Errorf("a token name is at most %d characters", MaxTokenNameLength)
	}
	if i.UserID == "" {
		return NewTokenInput{}, errors.New("a token needs an account")
	}

	// An unknown scope is refused rather than dropped. Dropping it would issue a token
	// with fewer permissions than the caller asked for and no indication that the request
	// was partly ignored, which is the failure mode where a client believes it holds an
	// access it does not.
	seen := map[string]bool{}
	for _, raw := range i.Scopes {
		scope := strings.TrimSpace(raw)
		if scope == "" {
			continue
		}
		if !validScope(scope) {
			return NewTokenInput{}, fmt.Errorf("the scope %q is not one this server grants; accepted scopes are %s",
				scope, strings.Join(ScopeSet, ", "))
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		normalized.Scopes = append(normalized.Scopes, scope)
	}
	if len(normalized.Scopes) == 0 {
		// A token with no scope would authenticate a caller and grant it nothing, which is
		// indistinguishable from a broken token. Defaulting to `repo` is what a client
		// asking for "a token" means.
		normalized.Scopes = []string{ScopeRepo}
	}
	// The order is fixed rather than the order the caller supplied, so a listing reads
	// the same way regardless of how it was requested.
	sortScopes(normalized.Scopes)
	return normalized, nil
}

// validScope reports whether a scope is one this build enforces.
func validScope(scope string) bool {
	for _, accepted := range ScopeSet {
		if scope == accepted {
			return true
		}
	}
	return false
}

// sortScopes orders a scope set against ScopeSet, so the stored value is stable.
func sortScopes(scopes []string) {
	for i := 1; i < len(scopes); i++ {
		for j := i; j > 0 && scopeRank(scopes[j]) < scopeRank(scopes[j-1]); j-- {
			scopes[j], scopes[j-1] = scopes[j-1], scopes[j]
		}
	}
}

// scopeRank is a scope's position in the accepted set, with unknown values last.
func scopeRank(scope string) int {
	for i, accepted := range ScopeSet {
		if scope == accepted {
			return i
		}
	}
	return len(ScopeSet)
}

// NewTokenRecord is what the store persists for a new token.
//
// It carries the digest and the prefix, and never the token itself: the raw value is
// returned to the caller once and is not part of anything stored.
type NewTokenRecord struct {
	ID          string
	UserID      string
	Name        string
	TokenPrefix string
	TokenHash   []byte
	Scopes      []string
}

// PrefixOf returns the display prefix of a token.
//
// It is exported because the administrator revoke command resolves a prefix the operator
// typed, and both sides must agree on how much of a token is stored in the clear.
func PrefixOf(token string) string {
	if len(token) <= tokenPrefixLength {
		return token
	}
	return token[:tokenPrefixLength]
}

// ValidateTokenShape reports whether a presented value could be a token this build
// issued.
//
// It is a shape check, not an authentication: a value of the wrong length cannot have
// come from the generator, so refusing it here avoids a database round trip on every
// malformed request. What authenticates is the digest comparison the store performs.
func ValidateTokenShape(value string) error {
	if value == "" {
		return ErrInvalidToken
	}
	if len(value) < MinTokenLength || len(value) > MaxTokenLength {
		return fmt.Errorf("%w: its length is not that of an issued token", ErrInvalidToken)
	}
	// Base64url without padding, which is what NewToken emits. Checking the alphabet here
	// means a value containing a character the encoding cannot produce — a space from a
	// copy-paste, a newline from a header — is refused as malformed rather than merely
	// not found.
	for i := 0; i < len(value); i++ {
		c := value[i]
		isUpper := c >= 'A' && c <= 'Z'
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if !isUpper && !isLower && !isDigit && c != '-' && c != '_' {
			return fmt.Errorf("%w: it contains a character the token encoding cannot produce", ErrInvalidToken)
		}
	}
	return nil
}

// newTokenIdentifier returns an identifier for a token row.
func newTokenIdentifier() string {
	// UUIDv7 rather than v4: it is time-ordered, so an index over it stays compact as rows
	// accumulate, and a token list is naturally ordered by creation.
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
