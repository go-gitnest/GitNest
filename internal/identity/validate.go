package identity

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/go-gitnest/GitNest/internal/identifier"
)

// Field limits. Usernames follow docs/architecture/urls-and-identifiers.md
// section 6.2, which takes GitHub's limits for compatibility. The password
// bounds come from docs/architecture/secrets.md: the upper bound exists so one
// request cannot ask for unbounded hashing work, and the lower bound matches
// GitHub's policy so a password accepted here is accepted there.
const (
	MinUsernameLength = 1
	MaxUsernameLength = 39

	// MinPasswordLength matches GitHub's documented minimum. It is recorded in
	// docs/architecture/secrets.md; changing it is a product decision, not a
	// local constant to tweak.
	MinPasswordLength = 8
	// MaxPasswordBytes is the ceiling from docs/architecture/secrets.md.
	MaxPasswordBytes = 1024

	// MaxEmailLength is the maximum address length from RFC 5321.
	MaxEmailLength = 254
	// MaxEmailLocalPart is the maximum local part length from RFC 5321.
	MaxEmailLocalPart = 64
)

// FoldName returns the comparison form of a name. The rule lives in
// internal/identifier so the repository domain folds names the same way.
func FoldName(name string) string {
	return identifier.Fold(name)
}

// Reserved reports whether a name is owned by a route.
func Reserved(name string) bool {
	return identifier.Reserved(name)
}

// ValidateUsername checks a username against the documented grammar, length, and
// reserved list. The display form is returned unchanged: normalization is case
// preservation plus a separate folded form, not a rewrite.
func ValidateUsername(username string) error {
	if username == "" {
		return fmt.Errorf("username must not be empty")
	}
	if !isASCII(username) {
		return fmt.Errorf("username may only contain ASCII letters, digits, and hyphens")
	}
	if len(username) < MinUsernameLength || len(username) > MaxUsernameLength {
		return fmt.Errorf("username must be between %d and %d characters", MinUsernameLength, MaxUsernameLength)
	}
	if !validUsernameGrammar(username) {
		return fmt.Errorf("username may contain letters, digits, and single interior hyphens; " +
			"it cannot start or end with a hyphen or contain two in a row")
	}
	if Reserved(username) {
		return fmt.Errorf("username %q is reserved", username)
	}
	return nil
}

// validUsernameGrammar implements the documented grammar
// `^[A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9]))*$`. It is a loop because Go's
// regular expressions have no lookahead.
func validUsernameGrammar(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case isAlnum(c):
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
			if name[i-1] == '-' {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// ValidateEmail checks an address before it is stored.
//
// It is a guard, not an RFC 5322 parser: it rejects what is certainly wrong and
// accepts what might be right. Deliverability is proven by the invitation flow,
// not by this function.
func ValidateEmail(email string) error {
	if email == "" {
		return fmt.Errorf("email address must not be empty")
	}
	if len(email) > MaxEmailLength {
		return fmt.Errorf("email address must be at most %d characters", MaxEmailLength)
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return fmt.Errorf("email address must not contain whitespace")
	}
	local, domain, found := strings.Cut(email, "@")
	if !found || strings.Contains(domain, "@") {
		return fmt.Errorf("email address must contain exactly one @")
	}
	if local == "" || len(local) > MaxEmailLocalPart {
		return fmt.Errorf("email address has an invalid local part")
	}
	if domain == "" || !strings.Contains(domain, ".") {
		return fmt.Errorf("email address must have a domain with a dot")
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return fmt.Errorf("email address has an invalid domain")
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("email address has an invalid domain")
		}
	}
	return nil
}

// NormalizeEmail returns the stored and compared form of an address. Addresses
// are compared case-insensitively in practice, and storing one form keeps the
// unique constraint meaningful.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidatePassword checks a plaintext password before it is hashed. Length is
// bounded and a minimum enforced; the password is not otherwise judged, because
// composition rules push users toward predictable patterns.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordBytes)
	}
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("password must not be only whitespace")
	}
	return nil
}
