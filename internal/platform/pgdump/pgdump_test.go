package pgdump_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/platform/pgdump"
)

// The DSN parsing tests matter more than they look: the parts are moved into the
// environment so the password never reaches the argument vector, and a parsing
// mistake would either leak it onto the command line or connect to the wrong
// database.

func TestAValidURLIsAccepted(t *testing.T) {
	valid := []string{
		"postgres://gitnest:secret@127.0.0.1:5432/gitnest?sslmode=disable",
		"postgresql://gitnest@localhost/gitnest",
		"postgres://user@host:5433/db?sslmode=require&connect_timeout=10",
		"postgres:///gitnest",
	}
	for _, dsn := range valid {
		t.Run(dsn, func(t *testing.T) {
			if err := (pgdump.Settings{DSN: dsn}).Validate(); err != nil {
				t.Errorf("a valid DSN was refused: %v", err)
			}
		})
	}
}

func TestAnUnusableDSNIsRefused(t *testing.T) {
	for _, test := range []struct {
		name string
		dsn  string
	}{
		{name: "empty", dsn: ""},
		{name: "whitespace", dsn: "   "},
		{name: "not a URL or keywords", dsn: "://nonsense"},
		{name: "the wrong scheme", dsn: "mysql://user@host/db"},
		{name: "no database", dsn: "postgres://user@host:5432"},
		{name: "a keyword form with no database", dsn: "host=localhost user=gitnest"},
		{name: "an unknown keyword", dsn: "host=localhost dbname=gitnest failover=true"},
		{name: "a field with no equals", dsn: "host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := (pgdump.Settings{DSN: test.dsn}).Validate(); err == nil {
				t.Errorf("%q was accepted", test.dsn)
			}
		})
	}
}

// TestTheKeywordFormIsAccepted covers the other shape an operator's environment may
// hold. Refusing it would only push them to work around this code.
func TestTheKeywordFormIsAccepted(t *testing.T) {
	dsn := "host=127.0.0.1 port=5432 user=gitnest password='a secret with spaces' dbname=gitnest sslmode=disable"
	if err := (pgdump.Settings{DSN: dsn}).Validate(); err != nil {
		t.Fatalf("a valid keyword DSN was refused: %v", err)
	}

	variables, err := pgdump.AsEnvironment(dsn)
	if err != nil {
		t.Fatalf("AsEnvironment: %v", err)
	}
	joined := strings.Join(variables, "\n")
	// A quoted value survives, spaces and all: libpq's grammar allows it, and a
	// password containing a space must not be silently truncated.
	if !strings.Contains(joined, "PGDATABASE=gitnest") {
		t.Errorf("the database was not extracted: %v", variables)
	}
	if !strings.Contains(joined, "PGSSLMODE=disable") {
		t.Errorf("the sslmode was not carried through: %v", variables)
	}
}

// TestThePasswordIsNeverRendered is the rule from administration.md: "No command
// accepts a password, token, or secret as a command-line argument, because arguments
// are visible in the process table and shell history."
//
// AsEnvironment is the exportable view of what the process receives, so it is the
// place a leak would be most likely — and it redacts instead.
func TestThePasswordIsNeverRendered(t *testing.T) {
	const secret = "hunter2-do-not-log-me"
	variables, err := pgdump.AsEnvironment("postgres://gitnest:" + secret + "@127.0.0.1:5432/gitnest")
	if err != nil {
		t.Fatalf("AsEnvironment: %v", err)
	}
	joined := strings.Join(variables, "\n")
	if strings.Contains(joined, secret) {
		t.Fatalf("the password was rendered:\n%s", joined)
	}
	if !strings.Contains(joined, "PGPASSWORD=<redacted>") {
		t.Errorf("the password variable is absent rather than redacted, which hides that one was configured:\n%s", joined)
	}
}

// TestSettingsValidateTheirTimeout keeps a negative bound from becoming a
// negative duration that cancels every invocation immediately.
func TestSettingsValidateTheirTimeout(t *testing.T) {
	good := pgdump.Settings{DSN: "postgres://user@host/gitnest"}
	if err := good.Validate(); err != nil {
		t.Errorf("valid settings were refused: %v", err)
	}
	bad := pgdump.Settings{DSN: "postgres://user@host/gitnest", Timeout: -1}
	if err := bad.Validate(); err == nil {
		t.Error("a negative timeout was accepted")
	}
}

// TestNewValidatesAtConstruction keeps a bad DSN at startup rather than at the
// moment an operator is trying to take a backup.
func TestNewValidatesAtConstruction(t *testing.T) {
	if _, err := pgdump.New(pgdump.Settings{DSN: "mysql://user@host/db"}); err == nil {
		t.Error("an unusable DSN was accepted at construction")
	}
	if _, err := pgdump.New(pgdump.Settings{DSN: "postgres://user@host/gitnest"}); err != nil {
		t.Errorf("a usable DSN was refused: %v", err)
	}
}

// TestAMissingProgramIsNamed covers the failure an operator meets first: the
// PostgreSQL client programs are not installed.
//
// It asserts against a binary name that cannot exist, so the message is checked
// without depending on whether this machine has `pg_dump`.
func TestAMissingProgramIsNamed(t *testing.T) {
	adapter, err := pgdump.New(pgdump.Settings{
		DSN:        "postgres://user@127.0.0.1:1/gitnest",
		DumpBinary: "gitnest-no-such-pg-dump-binary",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = adapter.Dump(context.Background(), &discard{})
	if err == nil {
		t.Fatal("a missing pg_dump was accepted")
	}
	if !errors.Is(err, pgdump.ErrBinaryNotFound) {
		t.Errorf("error = %v, want ErrBinaryNotFound", err)
	}
	// The message names the program, so an operator knows what to install.
	if !strings.Contains(err.Error(), "pg_dump") {
		t.Errorf("the error does not name the program: %v", err)
	}
}

// findProgram locates a PostgreSQL client program on PATH or in a conventional
// installation directory.
//
// The second half is what lets this test actually run on a machine where PostgreSQL
// was installed by an installer that did not add its `bin` directory to `PATH` — which
// is the default on Windows. A skip would otherwise hide the one code path that
// matters most: what an operator sees when the program runs and fails.
func findProgram(name string) (string, error) {
	if found, err := exec.LookPath(name); err == nil {
		return found, nil
	}
	for _, version := range []string{"18", "17", "16", "15"} {
		candidate := filepath.Join("C:\\Program Files\\PostgreSQL", version, "bin", name+".exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("the program is not installed")
}

// TestDumpIsRefusedWhenTheProgramCannotRun covers the path from the invocation to a
// reported error, without needing a database: the program exists but the connection
// cannot be made.
func TestDumpIsRefusedWhenTheProgramCannotRun(t *testing.T) {
	binary, err := findProgram("pg_dump")
	if err != nil {
		t.Skip("pg_dump is not installed")
	}

	// Port 1 has nothing listening, so the attempt fails at the connection.
	adapter, err := pgdump.New(pgdump.Settings{
		DSN: "postgres://user@127.0.0.1:1/gitnest", DumpBinary: binary,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = adapter.Dump(context.Background(), &discard{})
	if err == nil {
		t.Fatal("a dump against a dead port succeeded")
	}
	// The program's own diagnostics are carried, because "exit status 1" alone gives
	// an operator nothing to act on.
	if !strings.Contains(err.Error(), "pg_dump") && !strings.Contains(err.Error(), "connection") {
		t.Errorf("the error does not name the problem: %v", err)
	}
}

// discard is a writer that keeps nothing, so a failing dump cannot fill memory.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
