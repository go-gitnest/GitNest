package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/backup"
	"github.com/go-gitnest/GitNest/internal/buildinfo"
	"github.com/go-gitnest/GitNest/internal/config"
	"github.com/go-gitnest/GitNest/internal/platform/pgdump"
)

// The backup and restore commands.
//
// docs/architecture/administration.md names them — "`gitnest backup` /
// `gitnest restore` | Run the documented coordinated recovery-set procedure and
// verify it. | Server-local operator" — and requires them to be CLI-only: "Every
// command below is a subcommand of `cmd/gitnest`", and the console has "no API at
// all and is CLI-only (migration, backup, restore, key rotation)". Nothing here is
// reachable from an HTTP transport, and nothing here takes a credential on the
// command line: the database connection comes from the configured
// `GITNEST_DATABASE_URL`, which pgdump moves into the environment before invoking a
// program.
//
// The rules from the same document that shape the flags:
//
//  1. "No command accepts a password, token, or secret as a command-line argument."
//     So there is no `--database-url`; the environment supplies it.
//  2. "No command that changes data runs without an explicit confirmation or an
//     explicit `--apply` flag. The destructive default is dry-run." So `restore`
//     requires `--confirm`, and refuses without it.

// backupCommand captures a recovery set.
func backupCommand(ctx context.Context, args []string, streams stdio) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(streams.err)
	target := flags.String("target", "", "directory to write the recovery set into (required)")
	timeout := flags.Duration("timeout", pgdump.DefaultTimeout, "bound on one database invocation")
	flags.Usage = func() {
		fmt.Fprintln(streams.err, "usage: gitnest backup --target <directory> [--timeout 30m]")
		fmt.Fprintln(streams.err, "")
		fmt.Fprintln(streams.err, "Captures the database and the repository storage as one recovery set and")
		fmt.Fprintln(streams.err, "verifies it. The connection string comes from GITNEST_DATABASE_URL; it is")
		fmt.Fprintln(streams.err, "never taken from a command-line argument.")
		fmt.Fprintln(streams.err, "")
		fmt.Fprintln(streams.err, "The target must not be inside the repository root or the temporary root, and")
		fmt.Fprintln(streams.err, "it must not already hold a set.")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		flags.Usage()
		return errors.New("backup needs --target")
	}
	if *timeout <= 0 {
		return errors.New("the timeout must be positive")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := configureLogging(cfg); err != nil {
		return err
	}

	database, err := pgdump.New(pgdump.Settings{
		DSN: cfg.DatabaseURL, Timeout: *timeout,
		Log: func(message string, attributes ...any) { logf(streams, message, attributes...) },
	})
	if err != nil {
		return err
	}

	// The set is captured by an absolute path so the operator's `cd` cannot change
	// what a later verification means.
	absolute, err := filepath.Abs(*target)
	if err != nil {
		return fmt.Errorf("resolve the target: %w", err)
	}

	manifest, err := backup.Create(ctx, backup.Options{
		RepoRoot:     cfg.RepoRoot,
		TmpRoot:      cfg.TmpRoot,
		Target:       absolute,
		BuildVersion: buildinfo.Current().Version,
		Database:     database,
		Log:          func(message string, attributes ...any) { logf(streams, message, attributes...) },
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(streams.out, "recovery set written to %s\n", absolute)
	fmt.Fprintf(streams.out, "  capture time      %s\n", manifest.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(streams.out, "  gitnest build     %s\n", manifest.BuildVersion)
	fmt.Fprintf(streams.out, "  database schema   %s\n", manifest.SchemaVersion)
	fmt.Fprintf(streams.out, "  database dump     %d bytes, sha256 %s\n", manifest.Database.Bytes, short(manifest.Database.SHA256))
	fmt.Fprintf(streams.out, "  repositories      %d, %d files, %d bytes\n",
		len(manifest.Repositories), manifest.RepositoriesFiles, manifest.RepositoriesBytes)
	fmt.Fprintf(streams.out, "  verified          yes\n")
	fmt.Fprintln(streams.out, "")
	fmt.Fprintln(streams.out, "The instance secret key is NOT part of this set. Back it up separately:")
	fmt.Fprintln(streams.out, "a set that contained both the ciphertext and the key would protect nothing.")
	fmt.Fprintln(streams.out, "See docs/operations/backup-restore.md.")
	return nil
}

// restoreCommand restores a recovery set.
func restoreCommand(ctx context.Context, args []string, streams stdio) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(streams.err)
	source := flags.String("source", "", "recovery set to restore (required)")
	confirm := flags.Bool("confirm", false, "required: acknowledges that this overwrites the instance's data")
	timeout := flags.Duration("timeout", pgdump.DefaultTimeout, "bound on one database invocation")
	flags.Usage = func() {
		fmt.Fprintln(streams.err, "usage: gitnest restore --source <directory> --confirm [--timeout 30m]")
		fmt.Fprintln(streams.err, "")
		fmt.Fprintln(streams.err, "Restores a verified recovery set over this instance's database and repository")
		fmt.Fprintln(streams.err, "storage. The instance must be stopped: restoring is not safe against a")
		fmt.Fprintln(streams.err, "running server.")
		fmt.Fprintln(streams.err, "")
		fmt.Fprintln(streams.err, "--confirm is required. Without it the command refuses, because it")
		fmt.Fprintln(streams.err, "overwrites data and cannot be undone.")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source == "" {
		flags.Usage()
		return errors.New("restore needs --source")
	}
	if !*confirm {
		flags.Usage()
		return errors.New("restore overwrites this instance's data; pass --confirm to proceed")
	}
	if *timeout <= 0 {
		return errors.New("the timeout must be positive")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := configureLogging(cfg); err != nil {
		return err
	}

	database, err := pgdump.New(pgdump.Settings{
		DSN: cfg.DatabaseURL, Timeout: *timeout,
		Log: func(message string, attributes ...any) { logf(streams, message, attributes...) },
	})
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(*source)
	if err != nil {
		return fmt.Errorf("resolve the source: %w", err)
	}

	// The operator is told what is about to happen, including the capture time, so a
	// mistyped path is visible before anything is overwritten.
	manifest, err := backup.ReadManifest(absolute)
	if err != nil {
		return err
	}
	fmt.Fprintf(streams.out, "restoring the set captured at %s by gitnest %s\n",
		manifest.CreatedAt.Format(time.RFC3339), manifest.BuildVersion)
	fmt.Fprintf(streams.out, "  database schema   %s\n", manifest.SchemaVersion)
	fmt.Fprintf(streams.out, "  repositories      %d\n", len(manifest.Repositories))
	for _, note := range manifest.Notes {
		fmt.Fprintf(streams.out, "  note              %s\n", note)
	}
	fmt.Fprintln(streams.out, "")

	if err := backup.Restore(ctx, backup.RestoreOptions{
		Source:   absolute,
		RepoRoot: cfg.RepoRoot,
		TmpRoot:  cfg.TmpRoot,
		Database: database,
		Confirm:  true,
		Log:      func(message string, attributes ...any) { logf(streams, message, attributes...) },
	}); err != nil {
		return err
	}

	fmt.Fprintln(streams.out, "restore complete")
	fmt.Fprintln(streams.out, "")
	fmt.Fprintln(streams.out, "Next: run `gitnest migrate` and `gitnest admin check` against the restored")
	fmt.Fprintln(streams.out, "database, then follow the verification steps in docs/operations/backup-restore.md.")
	return nil
}

// verifyCommand checks a set without restoring it.
//
// It is a separate command because an operator's most common question is "is this
// backup usable", and answering it must not require touching the instance.
func verifyCommand(_ context.Context, args []string, streams stdio) error {
	flags := flag.NewFlagSet("backup-verify", flag.ContinueOnError)
	flags.SetOutput(streams.err)
	source := flags.String("source", "", "recovery set to verify (required)")
	flags.Usage = func() {
		fmt.Fprintln(streams.err, "usage: gitnest backup-verify --source <directory>")
		fmt.Fprintln(streams.err, "")
		fmt.Fprintln(streams.err, "Checks a recovery set against its manifest and reports every problem found.")
		fmt.Fprintln(streams.err, "It reads the set and changes nothing, so it is safe to run at any time.")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source == "" {
		flags.Usage()
		return errors.New("backup-verify needs --source")
	}

	absolute, err := filepath.Abs(*source)
	if err != nil {
		return fmt.Errorf("resolve the source: %w", err)
	}
	manifest, err := backup.ReadManifest(absolute)
	if err != nil {
		return err
	}

	problems, err := backup.Verify(absolute)
	if err != nil {
		return err
	}

	fmt.Fprintf(streams.out, "%s: captured %s by gitnest %s\n",
		absolute, manifest.CreatedAt.Format(time.RFC3339), manifest.BuildVersion)
	if len(problems) == 0 {
		fmt.Fprintln(streams.out, "the set is complete and consistent")
		return nil
	}

	// A problem about the set makes it unusable; a finding about the instance is
	// information, because the set holds everything that existed. The two are
	// separated, so an operator knows which one they are looking at.
	var inSet, aboutInstance []string
	for _, problem := range problems {
		if problem.InSet {
			inSet = append(inSet, problem.String())
		} else {
			aboutInstance = append(aboutInstance, problem.String())
		}
	}

	for _, detail := range aboutInstance {
		fmt.Fprintf(streams.err, "the instance was already inconsistent: %s\n", detail)
	}
	if len(inSet) == 0 {
		fmt.Fprintln(streams.out, "the set is complete; the findings above describe the instance at capture time")
		return nil
	}
	fmt.Fprintf(streams.err, "%d problem(s) make this set unusable:\n", len(inSet))
	for _, detail := range inSet {
		fmt.Fprintf(streams.err, "  %s\n", detail)
	}
	return fmt.Errorf("%w: %d problem(s)", backup.ErrSetIncomplete, len(inSet))
}

// logf renders a progress line as a single line on the error stream.
//
// Progress goes to stderr rather than stdout so a caller can read the summary from
// stdout without filtering the narration out.
func logf(streams stdio, message string, attributes ...any) {
	builder := &strings.Builder{}
	builder.WriteString("  ")
	builder.WriteString(message)
	for i := 0; i+1 < len(attributes); i += 2 {
		fmt.Fprintf(builder, " %v=%v", attributes[i], attributes[i+1])
	}
	fmt.Fprintln(streams.err, builder.String())
}

// short renders the first twelve characters of a digest, which is what an operator
// compares by eye.
func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12] + "…"
}
