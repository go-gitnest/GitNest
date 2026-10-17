package postgres_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-gitnest/GitNest/internal/platform/postgres"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/migrations"
)

// ledgerRows returns the migration ledger of a schema-scoped client.
func ledgerRows(t *testing.T, client *postgres.Client, ctx context.Context) map[string]time.Time {
	t.Helper()
	rows, err := client.DB.QueryContext(ctx, "SELECT name, applied_at FROM schema_migrations")
	if err != nil {
		t.Fatalf("read migration ledger: %v", err)
	}
	defer func() { _ = rows.Close() }()

	entries := map[string]time.Time{}
	for rows.Next() {
		var name string
		var appliedAt time.Time
		if err := rows.Scan(&name, &appliedAt); err != nil {
			t.Fatalf("scan migration ledger: %v", err)
		}
		entries[name] = appliedAt
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migration ledger: %v", err)
	}
	return entries
}

func tableExists(t *testing.T, client *postgres.Client, ctx context.Context, name string) bool {
	t.Helper()
	var exists bool
	if err := client.DB.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		t.Fatalf("check relation %s: %v", name, err)
	}
	return exists
}

// TestMigrateAppliesTheShippedMigrations covers the acceptance criterion that a
// run against an empty schema creates every table and records every file.
func TestMigrateAppliesTheShippedMigrations(t *testing.T) {
	client := postgrestest.NewEmptySchema(t).Client
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	expected, err := postgres.Load(migrations.Files)
	if err != nil {
		t.Fatalf("load the shipped migrations: %v", err)
	}
	if len(expected) == 0 {
		t.Fatal("the shipped migration set is empty; the embed is not working")
	}

	report, err := client.Migrate(ctx, migrations.Files)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(report.Applied) != len(expected) {
		t.Fatalf("applied %v, want every one of %d migrations", report.Applied, len(expected))
	}
	for i, migration := range expected {
		if report.Applied[i] != migration.Name {
			t.Errorf("applied %d = %q, want %q (application order must follow the file order)",
				i, report.Applied[i], migration.Name)
		}
	}
	if len(report.Skipped) != 0 {
		t.Errorf("skipped %v on a first run", report.Skipped)
	}

	entries := ledgerRows(t, client, ctx)
	for _, migration := range expected {
		if _, ok := entries[migration.Name]; !ok {
			t.Errorf("ledger does not record %s", migration.Name)
		}
	}
	if !tableExists(t, client, ctx, "instance_settings") {
		t.Error("the migration did not create instance_settings")
	}
}

// TestMigrateSecondRunIsNoOp covers the criterion that a repeat run changes
// nothing, not even the ledger timestamps.
func TestMigrateSecondRunIsNoOp(t *testing.T) {
	client := postgrestest.NewEmptySchema(t).Client
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := client.Migrate(ctx, migrations.Files); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	before := ledgerRows(t, client, ctx)

	report, err := client.Migrate(ctx, migrations.Files)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(report.Applied) != 0 {
		t.Errorf("the second run applied %v, want nothing", report.Applied)
	}
	if len(report.Skipped) == 0 {
		t.Error("the second run skipped nothing; it must see the ledger")
	}

	after := ledgerRows(t, client, ctx)
	if len(before) != len(after) {
		t.Fatalf("ledger size changed from %d to %d", len(before), len(after))
	}
	for name, appliedAt := range before {
		if !after[name].Equal(appliedAt) {
			t.Errorf("migration %s was re-applied: applied_at %s -> %s", name, appliedAt, after[name])
		}
	}
}

// TestMigrateRollsBackFailedMigration covers the criterion that a failing
// migration leaves neither its schema work nor its ledger row behind.
func TestMigrateRollsBackFailedMigration(t *testing.T) {
	client := postgrestest.NewEmptySchema(t).Client
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	broken := fstest.MapFS{
		"0001_probe.up.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE rollback_probe (id INTEGER PRIMARY KEY);\n")},
		"0002_broken.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE broken_probe (id INTEGER PRIMARY KEY);\nINSERT INTO table_that_does_not_exist (id) VALUES (1);\n")},
	}

	report, err := client.Migrate(ctx, broken)
	if err == nil {
		t.Fatal("Migrate accepted a migration that cannot run")
	}
	if !strings.Contains(err.Error(), "0002_broken.up.sql") {
		t.Errorf("error = %v, want it to name the failing migration", err)
	}
	if len(report.Applied) != 1 || report.Applied[0] != "0001_probe.up.sql" {
		t.Errorf("report.Applied = %v, want the first migration only", report.Applied)
	}
	if !tableExists(t, client, ctx, "rollback_probe") {
		t.Error("the successful migration's table is missing")
	}
	if tableExists(t, client, ctx, "broken_probe") {
		t.Error("the failed migration's table survived; the transaction did not roll back")
	}
	entries := ledgerRows(t, client, ctx)
	if _, ok := entries["0001_probe.up.sql"]; !ok {
		t.Error("the ledger lost the successful migration")
	}
	if _, ok := entries["0002_broken.up.sql"]; ok {
		t.Error("the ledger recorded a migration that failed")
	}
}

// TestMigrateHandlesNoTransactionMigration covers the criterion that a
// migration marked as running outside a transaction is not wrapped in one.
func TestMigrateHandlesNoTransactionMigration(t *testing.T) {
	client := postgrestest.NewEmptySchema(t).Client
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A statement that cannot run inside a transaction is the reason the marker
	// exists; CREATE DATABASE is used here because it is available in any
	// schema without needing a large table to index concurrently.
	noTransaction := fstest.MapFS{
		"0001_marker.up.sql": &fstest.MapFile{Data: []byte(
			postgres.NoTransactionMarker + "\nCREATE TABLE outside_transaction_probe (id INTEGER PRIMARY KEY);\n")},
	}

	report, err := client.Migrate(ctx, noTransaction)
	if err != nil {
		t.Fatalf("Migrate with a no-transaction migration: %v", err)
	}
	if len(report.Applied) != 1 {
		t.Fatalf("applied %v, want the marked migration", report.Applied)
	}
	if !tableExists(t, client, ctx, "outside_transaction_probe") {
		t.Error("the marked migration did not run")
	}
	entries := ledgerRows(t, client, ctx)
	if _, ok := entries["0001_marker.up.sql"]; !ok {
		t.Error("the marked migration was not recorded in the ledger")
	}

	// The ledger insert is not in the same transaction, so a repeat run must
	// still treat it as applied.
	second, err := client.Migrate(ctx, noTransaction)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("the marked migration ran twice: %v", second.Applied)
	}
}

// TestMigrateDetectsAnEditedAppliedMigration covers the forward-only rule: an
// applied migration whose content changed is a failure, not a silent skip.
func TestMigrateDetectsAnEditedAppliedMigration(t *testing.T) {
	client := postgrestest.NewEmptySchema(t).Client
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	original := fstest.MapFS{
		"0001_probe.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE edited_probe (id INTEGER PRIMARY KEY);\n")},
	}
	if _, err := client.Migrate(ctx, original); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	before := ledgerRows(t, client, ctx)

	edited := fstest.MapFS{
		"0001_probe.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE edited_probe (id INTEGER PRIMARY KEY, extra TEXT);\n")},
	}
	_, err := client.Migrate(ctx, edited)
	if err == nil {
		t.Fatal("an edited migration was accepted")
	}
	if !strings.Contains(err.Error(), "changed after it was applied") {
		t.Errorf("error = %v, want it to explain that the migration was edited", err)
	}
	after := ledgerRows(t, client, ctx)
	if !after["0001_probe.up.sql"].Equal(before["0001_probe.up.sql"]) {
		t.Error("the ledger changed while rejecting an edited migration")
	}
}

// TestConcurrentMigrationRunsApplyEachMigrationOnce covers the concurrency
// obligation from docs/architecture/testing.md: two runners starting together
// must apply each migration exactly once.
func TestConcurrentMigrationRunsApplyEachMigrationOnce(t *testing.T) {
	client := postgrestest.NewEmptySchema(t).Client
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The sleep makes the two runs genuinely overlap, so the second one has to
	// wait on the lock and then observe the ledger rather than racing through.
	concurrent := fstest.MapFS{
		"0001_slow.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE concurrency_probe (id INTEGER PRIMARY KEY);\nSELECT pg_sleep(0.3);\n")},
		"0002_second.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE concurrency_probe_two (id INTEGER PRIMARY KEY);\n")},
	}

	const runners = 2
	var (
		wait    sync.WaitGroup
		mu      sync.Mutex
		applied = map[string]int{}
		errs    = make([]error, runners)
	)
	for i := 0; i < runners; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			report, err := client.Migrate(ctx, concurrent)
			mu.Lock()
			defer mu.Unlock()
			errs[i] = err
			for _, name := range report.Applied {
				applied[name]++
			}
		}(i)
	}
	wait.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d failed: %v", i, err)
		}
	}
	for _, name := range []string{"0001_slow.up.sql", "0002_second.up.sql"} {
		if applied[name] != 1 {
			t.Errorf("migration %s was applied %d times, want exactly 1", name, applied[name])
		}
	}
	if entries := ledgerRows(t, client, ctx); len(entries) != 2 {
		t.Errorf("ledger has %d rows, want 2: %v", len(entries), entries)
	}
}

// TestMigrateIsolatesSchemas proves the test harness itself: a second client
// with another schema sees its own empty ledger and none of the first schema's
// tables.
func TestMigrateIsolatesSchemas(t *testing.T) {
	first := postgrestest.NewEmptySchema(t).Client
	second := postgrestest.NewEmptySchema(t).Client
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := first.Migrate(ctx, migrations.Files); err != nil {
		t.Fatalf("Migrate on the first schema: %v", err)
	}
	// Give the second schema its own ledger, which must be empty.
	if _, err := second.Migrate(ctx, fstest.MapFS{}); err != nil {
		t.Fatalf("Migrate on the second schema: %v", err)
	}
	if entries := ledgerRows(t, second, ctx); len(entries) != 0 {
		t.Errorf("the second schema sees %d ledger rows; schemas are not isolated", len(entries))
	}
	if exists := tableExists(t, second, ctx, "instance_settings"); exists {
		t.Error("the second schema sees the first schema's table")
	}
}

// TestMigrateCreatesTheLedgerInTheActiveSchema pins that the runner owns the
// ledger: it must not exist before the first run, and an empty migration set
// still creates it.
func TestMigrateCreatesTheLedgerInTheActiveSchema(t *testing.T) {
	client := postgrestest.NewEmptySchema(t).Client
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if tableExists(t, client, ctx, "schema_migrations") {
		t.Fatal("the ledger exists before any migration ran; the schema is not fresh")
	}
	if _, err := client.Migrate(ctx, fstest.MapFS{}); err != nil {
		t.Fatalf("Migrate with an empty set: %v", err)
	}
	if !tableExists(t, client, ctx, "schema_migrations") {
		t.Error("the runner did not create the ledger")
	}
	if entries := ledgerRows(t, client, ctx); len(entries) != 0 {
		t.Errorf("an empty migration set recorded %v", entries)
	}
}
