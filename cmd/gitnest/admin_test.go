package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/config"
	"github.com/go-gitnest/GitNest/internal/identity"
)

// scripted builds a prompter that reads the given lines without a terminal,
// which is how the command is driven by a script or a test.
func scripted(input string) (*prompter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &prompter{in: strings.NewReader(input), out: out}, out
}

func TestPrompterReadsCredentials(t *testing.T) {
	prompts, out := scripted("alice\nAlice@Example.com\nfirst entry\nfirst entry\n")

	username, err := prompts.line("Username: ")
	if err != nil {
		t.Fatalf("line: %v", err)
	}
	email, err := prompts.line("Email: ")
	if err != nil {
		t.Fatalf("line: %v", err)
	}
	password, err := prompts.newPassword()
	if err != nil {
		t.Fatalf("newPassword: %v", err)
	}

	if username != "alice" || email != "Alice@Example.com" || password != "first entry" {
		t.Fatalf("read %q / %q / %q, want the scripted answers", username, email, password)
	}
	if !strings.Contains(out.String(), "Password: ") || !strings.Contains(out.String(), "Confirm password: ") {
		t.Errorf("the prompts did not ask for the password twice: %q", out.String())
	}
}

// TestPrompterRetriesMismatchedPasswords keeps a typo from sending the operator
// back to the start of the command.
func TestPrompterRetriesMismatchedPasswords(t *testing.T) {
	prompts, out := scripted("first\nsecond\nsecond\nsecond\n")

	password, err := prompts.newPassword()
	if err != nil {
		t.Fatalf("newPassword: %v", err)
	}
	if password != "second" {
		t.Fatalf("password = %q, want the second attempt", password)
	}
	if !strings.Contains(out.String(), "Passwords did not match") {
		t.Errorf("the mismatch was not reported: %q", out.String())
	}
}

func TestPrompterGivesUpAfterRepeatedMismatch(t *testing.T) {
	prompts, _ := scripted("a\nb\nc\nd\ne\nf\n")
	if _, err := prompts.newPassword(); err == nil {
		t.Fatal("never-matching entries were accepted")
	}
}

// TestPrompterWithoutInputFailsInsteadOfHanging covers a pipe that closes
// immediately: the command must not wait forever and must not create an account
// from empty fields.
func TestPrompterWithoutInputFailsInsteadOfHanging(t *testing.T) {
	prompts, _ := scripted("")
	if _, err := prompts.line("Username: "); !errors.Is(err, errNoInput) {
		t.Fatalf("error = %v, want errNoInput", err)
	}
}

func TestPrompterAcceptsInputWithoutATrailingNewline(t *testing.T) {
	prompts, _ := scripted("alice")
	value, err := prompts.line("Username: ")
	if err != nil {
		t.Fatalf("line: %v", err)
	}
	if value != "alice" {
		t.Errorf("value = %q, want the final line without a newline", value)
	}
}

func TestRunAdminRejectsBadArguments(t *testing.T) {
	cfg := config.Config{}
	var out, errOut bytes.Buffer
	streams := stdio{out: &out, err: &errOut}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "no subcommand", args: nil},
		{name: "unknown subcommand", args: []string{"delete"}},
		{name: "unexpected argument", args: []string{"create", "extra"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := runAdmin(context.Background(), cfg, test.args, streams)
			if err == nil {
				t.Fatal("invalid arguments were accepted")
			}
			if !strings.Contains(err.Error(), "usage: gitnest admin create") {
				t.Errorf("error = %v, want the usage message", err)
			}
		})
	}
}

// TestPasswordIsNeverAnArgument pins the rule that the password cannot be
// supplied on the command line, where it would land in a shell history and in
// the process table — and that the command does not echo it back either.
func TestPasswordIsNeverAnArgument(t *testing.T) {
	const secret = "sup3rsecret"

	for _, args := range [][]string{
		{"create", "--password=" + secret},
		{"create", secret},
		{"--password=" + secret},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runAdmin(context.Background(), config.Config{}, args, stdio{out: &out, err: &errOut})
			if err == nil {
				t.Fatal("the command accepted an argument")
			}
			message := err.Error() + out.String() + errOut.String()
			if strings.Contains(message, secret) {
				t.Errorf("the password was echoed: %q", message)
			}
			if !strings.Contains(err.Error(), "usage: gitnest admin create") {
				t.Errorf("error = %v, want the usage message", err)
			}
		})
	}
}

func TestDescribeBootstrapErrorAdvisesTheOperator(t *testing.T) {
	for _, test := range []struct {
		err      error
		contains string
	}{
		{err: identity.ErrAdminExists, contains: "already has an administrator"},
		{err: identity.ErrSchemaNotMigrated, contains: "gitnest migrate"},
		{err: identity.ErrUsernameTaken, contains: "username is already taken"},
		{err: identity.ErrEmailTaken, contains: "email address is already in use"},
	} {
		t.Run(test.contains, func(t *testing.T) {
			got := describeBootstrapError(test.err)
			if got == nil || !strings.Contains(got.Error(), test.contains) {
				t.Fatalf("describeBootstrapError(%v) = %v, want a message containing %q", test.err, got, test.contains)
			}
		})
	}
}

func TestDescribeBootstrapErrorPassesUnknownErrorsThrough(t *testing.T) {
	original := errors.New("connection refused")
	if got := describeBootstrapError(original); got != original {
		t.Errorf("describeBootstrapError(%v) = %v, want the original error", original, got)
	}
}
