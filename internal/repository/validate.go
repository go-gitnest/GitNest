package repository

import (
	"fmt"
	"strings"

	"github.com/go-gitnest/GitNest/internal/identifier"
)

// Limits from docs/architecture/urls-and-identifiers.md section 6.3, which
// takes GitHub's limits for compatibility.
const (
	MinNameLength = 1
	MaxNameLength = 100
	// MaxDescriptionLength bounds a description that reaches the UI, as
	// docs/architecture/data-model.md requires. The value matches GitHub's.
	MaxDescriptionLength = 350
	// maxBranchNameLength is a sanity bound for a Git ref name. Git itself has
	// no small limit, but a ref name reaching the UI is bounded like any other
	// stored text.
	maxBranchNameLength = 255
)

// gitSuffix is reserved because `{owner}/{repo}.git` is the Git transport's
// path shape, so a repository literally named `foo.git` would make
// `{owner}/foo.git.git` ambiguous.
const gitSuffix = ".git"

// ValidateName checks a repository name against the documented grammar,
// length, and reserved list.
//
// The grammar is `^[A-Za-z0-9._-]+$` with at least one alphanumeric, no leading
// `-`, `.`, or `_`, no trailing `-`, `.`, or `_`, no `..`, and a folded form
// that is neither `.`/`..` nor ending in `.git`. It is implemented as a loop
// rather than a regular expression so each clause can report itself.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("repository name must not be empty")
	}
	if len(name) < MinNameLength || len(name) > MaxNameLength {
		return fmt.Errorf("repository name must be between %d and %d characters", MinNameLength, MaxNameLength)
	}
	if !isASCII(name) {
		return fmt.Errorf("repository name may only contain ASCII letters, digits, and . _ -")
	}

	hasAlphanumeric := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case isAlphanumeric(c):
			hasAlphanumeric = true
		case c == '-' || c == '.' || c == '_':
			// Separators are allowed in the interior only.
			if i == 0 || i == len(name)-1 {
				return fmt.Errorf("repository name cannot start or end with %q", string(c))
			}
		default:
			return fmt.Errorf("repository name may only contain ASCII letters, digits, and . _ -")
		}
	}
	if !hasAlphanumeric {
		return fmt.Errorf("repository name must contain at least one letter or digit")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("repository name cannot contain %q", "..")
	}

	folded := identifier.Fold(name)
	if folded == "." || folded == ".." {
		return fmt.Errorf("repository name cannot be %q", name)
	}
	if strings.HasSuffix(folded, gitSuffix) {
		return fmt.Errorf("repository name cannot end with %q", gitSuffix)
	}
	if identifier.Reserved(name) {
		return fmt.Errorf("repository name %q is reserved", name)
	}
	return nil
}

// FoldName returns the stored comparison form of a repository name.
func FoldName(name string) string {
	return identifier.Fold(name)
}

// ValidateBranchName checks a branch name against the Git ref rules named in
// docs/architecture/urls-and-identifiers.md section 6.3: no `..`, no leading or
// trailing `/`, no control characters, no `@{`, no `\`, and it must be a valid
// refname.
//
// "A valid refname" invokes Git's own additional restrictions. They are checked
// here, so an invalid name is rejected at the boundary rather than failing later
// inside the Git runner, and most of them apply per path component because a
// branch name may contain `/`.
func ValidateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("branch name must not be empty")
	}
	if len(name) > maxBranchNameLength {
		return fmt.Errorf("branch name must be at most %d characters", maxBranchNameLength)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("branch name cannot contain %q", "..")
	}
	if strings.Contains(name, "@{") {
		return fmt.Errorf("branch name cannot contain %q", "@{")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("branch name cannot start or end with %q", "/")
	}
	if strings.Contains(name, "//") {
		return fmt.Errorf("branch name cannot contain %q", "//")
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("branch name cannot end with %q", ".")
	}
	if name == "@" {
		return fmt.Errorf("branch name cannot be %q", "@")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("branch name cannot contain control characters")
		case c == ' ' || c == '~' || c == '^' || c == ':' || c == '?' ||
			c == '*' || c == '[' || c == '\\':
			return fmt.Errorf("branch name cannot contain %q", string(c))
		}
	}
	// The remaining rules are per component, because `/` separates them.
	for _, component := range strings.Split(name, "/") {
		if strings.HasPrefix(component, ".") {
			return fmt.Errorf("a branch name component cannot start with %q", ".")
		}
		if strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("a branch name component cannot end with %q", ".lock")
		}
	}
	return nil
}

// ValidateDescription bounds a description before it is stored.
func ValidateDescription(description string) error {
	if len(description) > MaxDescriptionLength {
		return fmt.Errorf("description must be at most %d characters", MaxDescriptionLength)
	}
	if strings.ContainsRune(description, 0) {
		return fmt.Errorf("description cannot contain a null byte")
	}
	return nil
}

// ParseVisibility validates a caller-supplied visibility, defaulting to private
// when none is given.
//
// Private-by-default is a product rule, not a convenience: a new repository is
// private unless its owner selects public (docs/product-goals.md).
func ParseVisibility(value string) (Visibility, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return VisibilityPrivate, nil
	case string(VisibilityPrivate):
		return VisibilityPrivate, nil
	case string(VisibilityPublic):
		return VisibilityPublic, nil
	default:
		return "", fmt.Errorf("visibility must be %q or %q", VisibilityPrivate, VisibilityPublic)
	}
}

func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return false
		}
	}
	return true
}
