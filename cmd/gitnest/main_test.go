package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/buildinfo"
)

func TestRunWithoutArgumentsPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), nil, stdio{out: &out, err: &errOut})

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if out.Len() != 0 {
		t.Errorf("usage went to stdout: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "usage: gitnest <command>") {
		t.Errorf("stderr = %q, want the usage line", errOut.String())
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"frobnicate"}, stdio{out: &out, err: &errOut})

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d for an unknown command", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q, want it to name the unknown command", errOut.String())
	}
	if !strings.Contains(errOut.String(), "usage: gitnest <command>") {
		t.Errorf("stderr = %q, want the usage to follow the error", errOut.String())
	}
}

func TestVersionCommandPrintsBuildIdentity(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"version"}, stdio{out: &out, err: &errOut})

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, errOut.String())
	}
	if !strings.Contains(out.String(), "gitnest "+buildinfo.Current().Version) {
		t.Errorf("stdout = %q, want the build identity", out.String())
	}
}

func TestVersionCommandRejectsArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"version", "extra"}, stdio{out: &out, err: &errOut})

	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if !strings.Contains(errOut.String(), "takes no arguments") {
		t.Errorf("stderr = %q, want it to explain the misuse", errOut.String())
	}
}

// TestUsageListsOnlyImplementedCommands keeps the help output honest: a command
// listed here must be dispatchable.
//
// Each command is actually invoked, so a run mode that would serve traffic must
// be given a configuration that fails fast and a context that cannot block the
// test: the database points at a closed port. The check is that the command was
// recognised, not which exit code it produced, because a command may legitimately
// report a usage error for missing arguments.
func TestUsageListsOnlyImplementedCommands(t *testing.T) {
	t.Setenv("GITNEST_DATABASE_URL", "postgres://gitnest@127.0.0.1:1/gitnest?sslmode=disable&connect_timeout=1")

	var usage bytes.Buffer
	run(context.Background(), nil, stdio{out: &usage, err: &usage})

	for _, candidate := range commands() {
		if !strings.Contains(usage.String(), candidate.name) {
			t.Errorf("command %q is dispatchable but missing from the usage output", candidate.name)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var stdout, stderr bytes.Buffer
		_ = run(ctx, []string{candidate.name}, stdio{out: &stdout, err: &stderr})
		cancel()

		if strings.Contains(stderr.String(), "unknown command") {
			t.Errorf("command %q is advertised in usage but is not dispatchable", candidate.name)
		}
	}
}
