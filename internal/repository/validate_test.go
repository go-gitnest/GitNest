package repository

import (
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/identifier"
)

func TestValidateNameAccepts(t *testing.T) {
	for _, name := range []string{
		"a", "repo", "my-repo", "my_repo", "my.repo", "Repo", "repo2",
		"a.b-c_d", "1", strings.Repeat("a", MaxNameLength),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateName(name); err != nil {
				t.Fatalf("ValidateName(%q): %v", name, err)
			}
		})
	}
}

func TestValidateNameRejects(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason string
	}{
		{name: "", reason: "empty"},
		{name: strings.Repeat("a", MaxNameLength+1), reason: "too long"},
		{name: "-repo", reason: "leading hyphen"},
		{name: ".repo", reason: "leading dot"},
		{name: "_repo", reason: "leading underscore"},
		{name: "repo-", reason: "trailing hyphen"},
		{name: "repo.", reason: "trailing dot"},
		{name: "repo_", reason: "trailing underscore"},
		{name: "re..po", reason: "double dot"},
		{name: ".", reason: "traversal"},
		{name: "..", reason: "traversal"},
		{name: "repo.git", reason: ".git suffix"},
		{name: "Repo.GIT", reason: ".git suffix, case-folded"},
		{name: "re/po", reason: "path separator"},
		{name: `re\po`, reason: "backslash"},
		{name: "re po", reason: "space"},
		{name: "repo\n", reason: "control character"},
		{name: "repo\x00", reason: "null byte"},
		{name: "存储库", reason: "non-ASCII"},
		{name: "café", reason: "non-ASCII"},
		{name: "---", reason: "no alphanumeric"},
		{name: "...", reason: "no alphanumeric"},
		{name: "admin", reason: "reserved"},
		{name: "ADMIN", reason: "reserved, case-folded"},
		{name: "settings", reason: "reserved"},
	} {
		t.Run(test.reason+" "+test.name, func(t *testing.T) {
			if err := ValidateName(test.name); err == nil {
				t.Fatalf("ValidateName(%q) accepted a name that should be rejected (%s)", test.name, test.reason)
			}
		})
	}
}

// TestValidateNameRejectsTraversalShapes is the security-relevant subset: no
// accepted name can be a path traversal, an absolute path, or a git object
// directory.
//
// Windows device names such as `con` are deliberately absent from this list.
// The documented grammar does not forbid them, and a repository name never
// reaches the filesystem: a storage path derives from the storage identifier,
// so there is nothing for a device name to collide with.
func TestValidateNameRejectsTraversalShapes(t *testing.T) {
	for _, name := range []string{
		".", "..", "...", ".git", "../etc", "..\\etc", "/etc/passwd",
		"a/../../etc", "a\\..\\b", ".ssh", "C:", "repo/",
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateName(name)
			if err == nil {
				t.Fatalf("ValidateName(%q) accepted a traversal shape", name)
			}
			// The message must not echo the input in a way that becomes a path.
			if strings.Contains(err.Error(), "/") || strings.Contains(err.Error(), `\`) {
				t.Errorf("the error message carries a path separator: %v", err)
			}
		})
	}
}

func TestValidateNameReportsTheRule(t *testing.T) {
	for _, test := range []struct {
		name    string
		expects string
	}{
		{name: "", expects: "must not be empty"},
		{name: "repo.git", expects: ".git"},
		{name: "admin", expects: "reserved"},
		{name: "re po", expects: "ASCII letters"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateName(test.name)
			if err == nil {
				t.Fatal("the name was accepted")
			}
			if !strings.Contains(err.Error(), test.expects) {
				t.Errorf("error = %v, want it to mention %q", err, test.expects)
			}
		})
	}
}

func TestFoldNameIsASCIIOnly(t *testing.T) {
	if got := FoldName("MyRepo"); got != "myrepo" {
		t.Errorf("FoldName = %q, want myrepo", got)
	}
	// Folding must agree with the shared rule, so the stored folded column and
	// an index lookup cannot disagree.
	if FoldName("Ünicode") != identifier.Fold("Ünicode") {
		t.Error("the repository fold disagrees with the shared identifier fold")
	}
}

func TestValidateBranchNameAccepts(t *testing.T) {
	for _, name := range []string{
		"main", "master", "feature/x", "release-1.0", "user/name/thing",
		"fix_thing", "v1", "a.b", "UPPER",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateBranchName(name); err != nil {
				t.Fatalf("ValidateBranchName(%q): %v", name, err)
			}
		})
	}
}

func TestValidateBranchNameRejects(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason string
	}{
		{name: "", reason: "empty"},
		{name: "a..b", reason: "double dot"},
		{name: "/main", reason: "leading slash"},
		{name: "main/", reason: "trailing slash"},
		{name: "a//b", reason: "double slash"},
		{name: "a@{b", reason: "at-brace"},
		{name: `a\b`, reason: "backslash"},
		{name: "a b", reason: "space"},
		{name: "a~b", reason: "tilde"},
		{name: "a^b", reason: "caret"},
		{name: "a:b", reason: "colon"},
		{name: "a?b", reason: "question mark"},
		{name: "a*b", reason: "asterisk"},
		{name: "a[b", reason: "bracket"},
		{name: "a\x01b", reason: "control character"},
		{name: "a\x7fb", reason: "delete character"},
		{name: "main.", reason: "trailing dot"},
		{name: "@", reason: "lone at"},
		{name: ".hidden", reason: "leading dot component"},
		{name: "a/.hidden", reason: "leading dot in a later component"},
		{name: "main.lock", reason: "lock suffix"},
		{name: "a/b.lock", reason: "lock suffix in a later component"},
		{name: strings.Repeat("a", maxBranchNameLength+1), reason: "too long"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			if err := ValidateBranchName(test.name); err == nil {
				t.Fatalf("ValidateBranchName(%q) accepted an invalid ref (%s)", test.name, test.reason)
			}
		})
	}
}

func TestValidateDescription(t *testing.T) {
	if err := ValidateDescription(""); err != nil {
		t.Errorf("an empty description was rejected: %v", err)
	}
	if err := ValidateDescription(strings.Repeat("a", MaxDescriptionLength)); err != nil {
		t.Errorf("a description at the limit was rejected: %v", err)
	}
	if err := ValidateDescription(strings.Repeat("a", MaxDescriptionLength+1)); err == nil {
		t.Error("an over-long description was accepted")
	}
	if err := ValidateDescription("has\x00null"); err == nil {
		t.Error("a description with a null byte was accepted")
	}
}

// TestParseVisibilityDefaultsToPrivate pins the documented product rule.
func TestParseVisibilityDefaultsToPrivate(t *testing.T) {
	for _, test := range []struct {
		value string
		want  Visibility
	}{
		{value: "", want: VisibilityPrivate},
		{value: "private", want: VisibilityPrivate},
		{value: "PRIVATE", want: VisibilityPrivate},
		{value: " private ", want: VisibilityPrivate},
		{value: "public", want: VisibilityPublic},
		{value: "Public", want: VisibilityPublic},
	} {
		t.Run(test.value, func(t *testing.T) {
			got, err := ParseVisibility(test.value)
			if err != nil {
				t.Fatalf("ParseVisibility(%q): %v", test.value, err)
			}
			if got != test.want {
				t.Errorf("ParseVisibility(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}

	for _, value := range []string{"internal", "secret", "0", "publik"} {
		t.Run("reject "+value, func(t *testing.T) {
			if _, err := ParseVisibility(value); err == nil {
				t.Errorf("ParseVisibility(%q) accepted an unknown visibility", value)
			}
		})
	}
}
