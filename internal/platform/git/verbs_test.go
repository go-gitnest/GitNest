package git

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRevisionAccepts(t *testing.T) {
	for _, revision := range []string{
		"HEAD",
		"head",
		"main",
		"main-2",
		"feature/deep/branch",
		"refs/heads/main",
		"refs/tags/v1.0.0",
		"v1.0.0",
		"aaaabbbbccccddddeeeeffff0000111122223333",
		strings.Repeat("a", 64),
		"HEAD~1",
		"HEAD^",
		"HEAD~10",
		"8f3c1a2",
	} {
		t.Run(revision, func(t *testing.T) {
			if err := ValidateRevision(revision); err != nil {
				t.Fatalf("ValidateRevision(%q): %v", revision, err)
			}
		})
	}
}

func TestValidateRevisionRejects(t *testing.T) {
	for _, test := range []struct {
		name     string
		revision string
	}{
		{name: "empty", revision: ""},
		{name: "option", revision: "--all"},
		{name: "short option", revision: "-h"},
		{name: "range", revision: "main..other"},
		{name: "range with three dots", revision: "main...other"},
		{name: "revision with a path", revision: "HEAD:etc/passwd"},
		{name: "reflog form", revision: "@{1}"},
		{name: "upstream form", revision: "main@{upstream}"},
		{name: "empty component", revision: "refs//heads/main"},
		{name: "leading slash", revision: "/main"},
		{name: "trailing slash", revision: "main/"},
		{name: "space", revision: "main branch"},
		{name: "newline", revision: "main\nother"},
		{name: "null byte", revision: "main\x00"},
		{name: "backslash", revision: `main\other`},
		{name: "glob", revision: "refs/heads/*"},
		{name: "question mark", revision: "mai?"},
		{name: "bracket", revision: "mai[n]"},
		{name: "dollar", revision: "$(whoami)"},
		{name: "semicolon", revision: "main;rm -rf /"},
		{name: "pipe", revision: "main|x"},
		{name: "redirect", revision: "main>file"},
		{name: "backtick form", revision: "main`x`"},
		{name: "over-long", revision: strings.Repeat("a", maxRevisionLength+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateRevision(test.revision); err == nil {
				t.Fatalf("ValidateRevision(%q) accepted the revision", test.revision)
			}
		})
	}
}

func TestValidateRepositoryPathAccepts(t *testing.T) {
	for _, path := range []string{
		"",
		"README.md",
		"src/main.go",
		"a/b/c/d.rs",
		"docs/api reference.md",
		".github/workflows/ci.yml",
		"file.with.dots.txt",
		"kebab-case_name",
	} {
		t.Run(path, func(t *testing.T) {
			if err := ValidateRepositoryPath(path); err != nil {
				t.Fatalf("ValidateRepositoryPath(%q): %v", path, err)
			}
		})
	}
}

func TestValidateRepositoryPathRejects(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "absolute", path: "/etc/passwd"},
		{name: "traversal", path: "../secrets"},
		{name: "embedded traversal", path: "a/../../b"},
		{name: "tolerated-dot component", path: "a/./b"},
		{name: "trailing slash", path: "a/b/"},
		{name: "double slash", path: "a//b"},
		{name: "leading dash", path: "-flag"},
		{name: "ignored option style", path: "--output=/etc/passwd"},
		{name: "windows separator", path: `..\..\windows`},
		{name: "drive letter", path: `C:\Windows`},
		{name: "null byte", path: "a\x00b"},
		{name: "newline", path: "a\nb"},
		{name: "escape", path: "a\x1b[31mb"},
		{name: "over-long", path: strings.Repeat("a", maxRepositoryPathLength+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateRepositoryPath(test.path); err == nil {
				t.Fatalf("ValidateRepositoryPath(%q) accepted the path", test.path)
			}
		})
	}
}

func TestValidateCount(t *testing.T) {
	if err := ValidateCount(10, 100); err != nil {
		t.Errorf("ValidateCount(10, 100): %v", err)
	}
	if err := ValidateCount(100, 100); err != nil {
		t.Errorf("ValidateCount(100, 100): %v", err)
	}
	for _, count := range []int{0, -1, 101} {
		if err := ValidateCount(count, 100); err == nil {
			t.Errorf("ValidateCount(%d, 100) accepted the count", count)
		}
	}
}

func TestInspectionErrorsAreTheInvalidArgumentSentinel(t *testing.T) {
	// The transport maps ErrInvalidArgument onto a request problem, so a
	// validation failure has to be recognisable rather than an anonymous error.
	for _, err := range []error{
		ValidateRevision("--all"),
		ValidateRepositoryPath("../x"),
		ValidateCount(0, 10),
	} {
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("error %v is not ErrInvalidArgument", err)
		}
	}
}
