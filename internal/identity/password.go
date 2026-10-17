package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters, from docs/architecture/secrets.md: 64 MiB of memory,
// three iterations, two lanes, a 16-byte salt, and a 32-byte key.
//
// They are recorded in every hash, so raising them does not invalidate existing
// credentials: an old hash still verifies with the parameters it carries.
const (
	argon2Memory      uint32 = 64 * 1024
	argon2Iterations  uint32 = 3
	argon2Parallelism uint8  = 2
	argon2SaltLength         = 16
	argon2KeyLength          = 32
)

// Bounds accepted when verifying a stored hash. They stop a corrupted or
// tampered hash from asking for unbounded memory and CPU, which is the
// "parameters are bounded" requirement in docs/architecture/secrets.md.
const (
	maxVerifyMemory      = 1 << 20 // KiB, that is 1 GiB
	maxVerifyIterations  = 16
	maxVerifyParallelism = 16
)

// Hasher turns a plaintext password into a self-describing hash.
type Hasher interface {
	Hash(password string) (string, error)
}

// Argon2idHasher hashes passwords with Argon2id and encodes them in the PHC
// string format:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
type Argon2idHasher struct{}

// Hash returns the PHC-encoded Argon2id hash of password.
func (Argon2idHasher) Hash(password string) (string, error) {
	salt := make([]byte, argon2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate a password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argon2Iterations, argon2Memory, argon2Parallelism, argon2KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Iterations, argon2Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches a PHC-encoded Argon2id hash.
// The comparison is constant time, and the hash's parameters are bounded before
// any work is done.
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, want, err := decodeArgon2id(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argon2Params struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

func decodeArgon2id(encoded string) (argon2Params, []byte, []byte, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" || fields[1] != "argon2id" {
		return argon2Params{}, nil, nil, errors.New("password hash is not a PHC-encoded Argon2id hash")
	}
	version, err := strconv.Atoi(strings.TrimPrefix(fields[2], "v="))
	if err != nil || version != argon2.Version {
		return argon2Params{}, nil, nil, errors.New("password hash uses an unsupported Argon2 version")
	}
	params, err := parseArgon2Params(fields[3])
	if err != nil {
		return argon2Params{}, nil, nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil || len(salt) == 0 {
		return argon2Params{}, nil, nil, errors.New("password hash has an invalid salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(key) == 0 {
		return argon2Params{}, nil, nil, errors.New("password hash has an invalid key")
	}
	return params, salt, key, nil
}

func parseArgon2Params(field string) (argon2Params, error) {
	var memory, iterations, parallelism int
	for _, part := range strings.Split(field, ",") {
		name, value, found := strings.Cut(part, "=")
		if !found {
			return argon2Params{}, errors.New("password hash has invalid parameters")
		}
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			return argon2Params{}, errors.New("password hash has invalid parameters")
		}
		switch name {
		case "m":
			memory = number
		case "t":
			iterations = number
		case "p":
			parallelism = number
		default:
			return argon2Params{}, fmt.Errorf("password hash has an unknown parameter %q", name)
		}
	}
	if memory == 0 || iterations == 0 || parallelism == 0 {
		return argon2Params{}, errors.New("password hash is missing parameters")
	}
	if memory > maxVerifyMemory || iterations > maxVerifyIterations || parallelism > maxVerifyParallelism {
		return argon2Params{}, errors.New("password hash parameters exceed the accepted bounds")
	}
	return argon2Params{memory: uint32(memory), iterations: uint32(iterations), parallelism: uint8(parallelism)}, nil
}
