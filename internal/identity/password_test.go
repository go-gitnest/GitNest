package identity

import (
	"strings"
	"testing"
)

const testPassword = "correct horse battery staple"

func TestArgon2idHashRoundTrips(t *testing.T) {
	encoded, err := Argon2idHasher{}.Hash(testPassword)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=") {
		t.Fatalf("hash is not a PHC Argon2id string: %q", encoded)
	}
	// The parameters must be recorded, so they can be raised later without
	// invalidating credentials (docs/architecture/secrets.md).
	for _, parameter := range []string{"m=65536", "t=3", "p=2"} {
		if !strings.Contains(encoded, parameter) {
			t.Errorf("hash does not record %s: %q", parameter, encoded)
		}
	}

	ok, err := VerifyPassword(encoded, testPassword)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword rejected the correct password")
	}
}

func TestArgon2idHashIsSalted(t *testing.T) {
	first, err := Argon2idHasher{}.Hash(testPassword)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	second, err := Argon2idHasher{}.Hash(testPassword)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if first == second {
		t.Error("hashing the same password twice produced the same hash")
	}
}

func TestVerifyPasswordRejectsAWrongPassword(t *testing.T) {
	encoded, err := Argon2idHasher{}.Hash(testPassword)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ok, err := VerifyPassword(encoded, testPassword+"x")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("VerifyPassword accepted a wrong password")
	}
}

// TestVerifyPasswordAcceptsADifferentlyParameterisedHash pins forward
// compatibility: the parameters travel inside the hash, so raising them later
// does not invalidate an existing credential.
func TestVerifyPasswordAcceptsADifferentlyParameterisedHash(t *testing.T) {
	const older = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	if _, err := VerifyPassword(older, testPassword); err != nil {
		t.Fatalf("a differently parameterised hash was rejected: %v", err)
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, encoded := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=13$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA",
		"$argon2id$v=19$m=abc,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2,x=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=0,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
	} {
		if _, err := VerifyPassword(encoded, testPassword); err == nil {
			t.Errorf("VerifyPassword(%q) accepted a malformed hash", encoded)
		}
	}
}

// TestVerifyPasswordBoundsTheParameters stops a tampered hash from asking for
// unbounded memory or CPU.
func TestVerifyPasswordBoundsTheParameters(t *testing.T) {
	for _, encoded := range []string{
		"$argon2id$v=19$m=2000000,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=99,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=99$c2FsdA$aGFzaA",
	} {
		if _, err := VerifyPassword(encoded, testPassword); err == nil {
			t.Errorf("VerifyPassword(%q) accepted out-of-bounds parameters", encoded)
		}
	}
}
