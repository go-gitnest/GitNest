package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// tokenBytes is the entropy behind session, CSRF, and invitation tokens.
//
// Thirty-two bytes (256 bits) is far beyond guessing range, which is why these
// values are stored as a fast SHA-256 digest instead of an Argon2id hash: a
// password needs deliberate slowness, a random token does not
// (docs/architecture/secrets.md).
const tokenBytes = 32

// NewToken returns a freshly generated opaque token and the digest to store.
// The raw value is returned to the caller once and never persisted.
func NewToken() (token string, digest []byte, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate a token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken returns the digest to store for a raw token. It is deterministic so
// a presented token can be looked up by digest, which is the point of hashing
// rather than encrypting here.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// DigestsMatch compares two digests in constant time.
//
// A CSRF token is verified this way: the session is already resolved, so the
// comparison must not leak how much of the presented token was correct
// (docs/architecture/secrets.md).
func DigestsMatch(presented, stored []byte) bool {
	return subtle.ConstantTimeCompare(presented, stored) == 1
}
