package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/go-gitnest/GitNest/internal/app"
	"github.com/go-gitnest/GitNest/internal/config"
	"github.com/go-gitnest/GitNest/internal/platform/postgres"
	"github.com/go-gitnest/GitNest/migrations"
)

// migrateCommand is the migration entry point, in three modes.
//
// docs/architecture/packaging.md section 11 gives the upgrade procedure, and step 4
// names two of these modes as the preflight: "`gitnest migrate --status` reports the
// applied and expected app versions; `gitnest migrate --dry-run` lists the migrations
// that would be applied and exits non-zero when the database is newer than the
// binary." The bare `gitnest migrate` is step 5.
//
// The two preflight modes are **read-only** and safe against a production database,
// which is what makes them usable as a gate: they never create the ledger, never take
// the migration lock, and never apply anything. Step 5 explicitly warns that "a
// migration never runs from a unit's `ExecStartPre` without the operator's presence" —
// so a preflight a unit *can* run is the shape the document wants, and it is the shape
// implemented here.
func migrateCommand(ctx context.Context, cfg config.Config, args []string, streams stdio) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(streams.err)
	status := flags.Bool("status", false, "report the applied and pending migrations and change nothing")
	dryRun := flags.Bool("dry-run", false, "list the migrations that would be applied and change nothing")
	flags.Usage = func() {
		fmt.Fprintln(streams.err, "usage: gitnest migrate [--status | --dry-run]")
		fmt.Fprintln(streams.err, "")
		fmt.Fprintln(streams.err, "  (no flag)   apply the pending migrations, taking the migration lock")
		fmt.Fprintln(streams.err, "  --status    report what is applied and what is pending; changes nothing")
		fmt.Fprintln(streams.err, "  --dry-run   list what would be applied; exits non-zero when the database")
		fmt.Fprintln(streams.err, "              is ahead of this binary; changes nothing")
		fmt.Fprintln(streams.err, "")
		fmt.Fprintln(streams.err, "--status and --dry-run are safe against a production database and are the")
		fmt.Fprintln(streams.err, "preflight the upgrade procedure documents.")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("migrate takes no positional arguments, got %q", flags.Arg(0))
	}
	if *status && *dryRun {
		// They are the same read; asking for both is a mistake rather than a request
		// for two reports, because the output would be identical.
		return errors.New("--status and --dry-run are the same report; pass one")
	}

	if *status || *dryRun {
		return reportMigrationStatus(ctx, cfg, streams)
	}

	report, err := app.Migrate(ctx, cfg, migrations.Files)
	if err != nil {
		return err
	}
	logf(streams, "migrations applied", "applied", len(report.Applied), "skipped", len(report.Skipped))
	fmt.Fprintf(streams.out, "applied %d migration(s), skipped %d\n",
		len(report.Applied), len(report.Skipped))
	return nil
}

// reportMigrationStatus prints the preflight report.
//
// Both flags share this because they are the same read — the difference the document
// draws is in what the operator wants from it, and the exit status carries that: a
// database ahead of the binary fails either way, because continuing would run code
// against a schema it was not written for.
func reportMigrationStatus(ctx context.Context, cfg config.Config, streams stdio) error {
	// The settings mirror `app.Migrate`'s, so the preflight reaches the database
	// through the same bounded pool as the command it is a preflight for.
	client, err := postgres.Open(ctx, cfg.DatabaseURL, app.DatabaseSettings(cfg))
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	current, err := client.Check(ctx, migrations.Files)
	if err != nil {
		// The status is printed before the error is returned, because it names which
		// migrations are the problem and an operator needs that, not only the fact
		// that there is one.
		describeMigrationStatus(streams, current)
		return err
	}

	describeMigrationStatus(streams, current)
	if current.UpToDate() {
		fmt.Fprintln(streams.out, "the database and this binary agree; nothing to apply")
	} else {
		fmt.Fprintf(streams.out, "%d migration(s) would be applied by `gitnest migrate`\n", len(current.Pending))
	}
	return nil
}

// describeMigrationStatus writes the report both modes produce.
func describeMigrationStatus(streams stdio, status postgres.Status) {
	fmt.Fprintln(streams.out, "migration status")
	for _, line := range status.Describe() {
		fmt.Fprintln(streams.out, line)
	}
}
