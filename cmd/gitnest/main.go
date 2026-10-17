// Command gitnest is the GitNest entry point. It parses a command, wires the
// process-level concerns every command needs, and dispatches to the run mode or
// administration action that owns the work. It contains no product logic.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-gitnest/GitNest/internal/app"
	"github.com/go-gitnest/GitNest/internal/buildinfo"
	"github.com/go-gitnest/GitNest/internal/config"
	"github.com/go-gitnest/GitNest/internal/platform/logging"
)

// Exit codes. A usage problem is distinguished from a command failure so a
// script can tell a mistyped invocation from a run that started and failed.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// stdio carries the streams a command writes to, so a command can be exercised
// by a test without touching the real process streams.
type stdio struct {
	out io.Writer
	err io.Writer
}

// command is one run mode or administration action.
//
// args are the arguments after the command name. ctx is cancelled when the
// process receives an interrupt or termination signal.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, streams stdio) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The first signal cancels ctx and the command starts draining. Restoring
	// the default handler at that moment means a second signal terminates the
	// process immediately instead of being absorbed by the drain deadline.
	go func() {
		<-ctx.Done()
		stop()
	}()

	os.Exit(run(ctx, os.Args[1:], stdio{out: os.Stdout, err: os.Stderr}))
}

// run dispatches one invocation and returns the process exit code.
func run(ctx context.Context, args []string, streams stdio) int {
	if len(args) == 0 {
		printUsage(streams.err)
		return exitUsage
	}
	name := args[0]
	for _, candidate := range commands() {
		if candidate.name != name {
			continue
		}
		if err := candidate.run(ctx, args[1:], streams); err != nil {
			fmt.Fprintf(streams.err, "gitnest: %v\n", err)
			return exitError
		}
		return exitOK
	}
	fmt.Fprintf(streams.err, "gitnest: unknown command %q\n", name)
	printUsage(streams.err)
	return exitUsage
}

// commands lists every command this build supports. A run mode is added here by
// the workstream that implements it, so the usage output never advertises
// something that does not run.
func commands() []command {
	return []command{
		{
			name:    "serve",
			summary: "run the HTTP server",
			run: func(ctx context.Context, args []string, _ stdio) error {
				if len(args) > 0 {
					return fmt.Errorf("serve takes no arguments, got %q", args[0])
				}
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				if err := configureLogging(cfg); err != nil {
					return err
				}
				return app.New(cfg).Run(ctx)
			},
		},
		{
			name:    "migrate",
			summary: "apply pending database migrations, or report what would be applied",
			run: func(ctx context.Context, args []string, streams stdio) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				if err := configureLogging(cfg); err != nil {
					return err
				}
				return migrateCommand(ctx, cfg, args, streams)
			},
		},
		{
			name:    "backup",
			summary: "capture the database and repository storage as one verified recovery set",
			run:     backupCommand,
		},
		{
			name:    "backup-verify",
			summary: "check a recovery set and report every problem, changing nothing",
			run:     verifyCommand,
		},
		{
			name:    "restore",
			summary: "restore a verified recovery set (requires --confirm; stop the server first)",
			run:     restoreCommand,
		},
		{
			name:    "admin",
			summary: "administrative commands; see `gitnest admin create`",
			run: func(ctx context.Context, args []string, streams stdio) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				return runAdmin(ctx, cfg, args, streams)
			},
		},
		{
			name:    "health",
			summary: "probe this instance's own HTTP endpoint and exit non-zero when it is unhealthy",
			run:     healthCommand,
		},
		{
			name:    "version",
			summary: "print version, commit, and build information",
			run: func(_ context.Context, args []string, streams stdio) error {
				if len(args) > 0 {
					return fmt.Errorf("version takes no arguments, got %q", args[0])
				}
				fmt.Fprintln(streams.out, buildinfo.Current().String())
				return nil
			},
		},
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: gitnest <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, candidate := range commands() {
		fmt.Fprintf(w, "  %-10s %s\n", candidate.name, candidate.summary)
	}
}

// configureLogging installs the process logger from configuration and
// registers every secret the configuration carries, so a later error that
// echoes the connection string cannot leak its password.
//
// Records go to standard error: a command's result (a version string, a
// migration summary) is data on standard output, and mixing log lines into it
// would make that output unparseable.
func configureLogging(cfg config.Config) error {
	level, err := logging.ParseLevel(string(cfg.LogLevel))
	if err != nil {
		return err
	}
	logging.DefaultRedactor().Register(cfg.Secrets()...)
	slog.SetDefault(logging.Configure(os.Stderr, level, logging.DefaultRedactor()))
	return nil
}
