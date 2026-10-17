// Package postgrestest is the harness every database-backed test uses.
//
// It exists so that three rules are enforced in one place instead of being
// re-implemented, differently, in each test file:
//
//  1. A test that needs PostgreSQL skips itself, naming the variable that would
//     enable it, unless GITNEST_TEST_REQUIRE says the database is required — in
//     which case a missing database fails the run instead of silently skipping
//     everything.
//  2. Each test works in its own uniquely named schema on the maintenance
//     database, so tests in one package can run in parallel without seeing each
//     other's rows. The schema is dropped afterwards, never the database:
//     DROP DATABASE can block on a signal barrier on some PostgreSQL 18
//     installations.
//  3. The schema is dropped even when the test fails, because the drop is
//     registered with t.Cleanup.
package postgrestest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/platform/postgres"
	"github.com/go-gitnest/GitNest/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Environment variables that control the harness.
const (
	// EnvDatabaseURL names a maintenance connection: a postgres:// URL whose
	// user may create schemas. Without it every database-backed test skips.
	EnvDatabaseURL = "GITNEST_TEST_DATABASE_URL"
	// EnvRequire is a comma-separated list of what the run must have, for
	// example "postgres". A test that would skip fails instead, so a
	// misconfigured CI job cannot report success while running nothing.
	EnvRequire = "GITNEST_TEST_REQUIRE"
	// EnvConcurrency caps how many schemas one process holds at a time.
	EnvConcurrency = "GITNEST_TEST_DB_CONCURRENCY"
)

// operationTimeout bounds every setup step, so a hung server fails a test
// instead of stalling the suite.
const operationTimeout = 60 * time.Second

// schemaPrefix marks the schemas this harness owns, so a leak is recognisable.
const schemaPrefix = "gitnest_test_"

// Schema is a private schema for one test, with a client scoped to it.
type Schema struct {
	// Name is the schema, generated per test and quoted in every statement the
	// harness runs.
	Name string
	// Client is a pool whose connections use Name as their search path. Its
	// size is bounded so a parallel suite cannot exhaust the server.
	Client *postgres.Client
}

// Require returns the maintenance connection URL.
//
// It skips the test when the URL is not configured, unless GITNEST_TEST_REQUIRE
// lists postgres or all, in which case it fails: a required integration test
// that silently skips is how a suite goes green without testing anything.
func Require(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(EnvDatabaseURL))
	if dsn == "" {
		if Required("postgres") {
			// The message names a committed document, so a contributor who hits this from a CI log finds
			// an answer rather than a path that does not exist in the checkout.
			t.Fatalf("%s requires postgres, but %s is not set; see README.md under Development",
				EnvRequire, EnvDatabaseURL)
		}
		t.Skipf("set %s to run PostgreSQL integration tests", EnvDatabaseURL)
	}
	return dsn
}

// Required reports whether the environment demands a capability.
func Required(capability string) bool {
	for _, entry := range strings.Split(os.Getenv(EnvRequire), ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "all" || entry == strings.ToLower(capability) {
			return true
		}
	}
	return false
}

// NewSchema creates a schema and applies the shipped migrations through the
// production runner. It is the common case: a test starts from the schema the
// application would have.
func NewSchema(t *testing.T) *Schema {
	t.Helper()
	return newSchema(t, true)
}

// NewEmptySchema creates a schema with nothing in it, for tests that drive the
// migration runner themselves and need to observe it from scratch.
func NewEmptySchema(t *testing.T) *Schema {
	t.Helper()
	return newSchema(t, false)
}

func newSchema(t *testing.T, applyMigrations bool) *Schema {
	t.Helper()
	dsn := Require(t)

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open the maintenance connection: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("connect to the maintenance database: %v", err)
	}

	// The slot is released last, after the schema is gone, so the cap counts
	// schemas that actually exist.
	release := acquireSlot(t)
	t.Cleanup(release)
	t.Cleanup(func() { _ = admin.Close() })

	name := schemaPrefix + randomSuffix(t)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoteIdentifier(name)); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	// Registered before the client is opened, so a failure there still cleans up.
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), operationTimeout)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP SCHEMA IF EXISTS "+quoteIdentifier(name)+" CASCADE"); err != nil {
			// Reported rather than fatal: a leftover schema is an operator
			// problem, not a reason to fail an otherwise passing test.
			t.Logf("warning: could not drop test schema %s: %v", name, err)
		}
	})

	client, err := postgres.Open(ctx, scopedDSN(t, dsn, name), testPoolSettings())
	if err != nil {
		t.Fatalf("open a client for schema %s: %v", name, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if applyMigrations {
		if _, err := client.Migrate(ctx, migrations.Files); err != nil {
			t.Fatalf("apply migrations to schema %s: %v", name, err)
		}
	}
	return &Schema{Name: name, Client: client}
}

// testPoolSettings bounds a test's pool so a parallel suite stays within the
// server's connection limit.
func testPoolSettings() postgres.Settings {
	return postgres.Settings{
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
		AcquireTimeout:  10 * time.Second,
	}
}

// scopedDSN returns the maintenance URL with search_path set to the schema.
func scopedDSN(t *testing.T, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvDatabaseURL, err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		t.Fatalf("%s must be a postgres:// URL, got scheme %q", EnvDatabaseURL, parsed.Scheme)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// slots is the process-wide concurrency cap, created on first use.
var (
	slots     chan struct{}
	slotsOnce bool
)

// acquireSlot takes one concurrency slot and returns the release function. With
// no cap configured it returns a no-op, so a caller never has to check.
//
// The cap is per process: parallel test binaries each get their own, because
// coordinating across processes would need a server-side lock. CI sets the
// variable low enough that the packages it runs in parallel still fit.
func acquireSlot(t *testing.T) func() {
	t.Helper()
	limit := concurrencyLimit()
	if limit <= 0 {
		return func() {}
	}
	if !slotsOnce {
		slots = make(chan struct{}, limit)
		slotsOnce = true
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }
	case <-time.After(operationTimeout):
		t.Fatalf("waited %s for a database slot; %d are in use in this process", operationTimeout, limit)
		return func() {}
	}
}

func concurrencyLimit() int {
	raw := strings.TrimSpace(os.Getenv(EnvConcurrency))
	if raw == "" {
		return 0
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0
	}
	return limit
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generate a schema name: %v", err)
	}
	return hex.EncodeToString(buf)
}

// quoteIdentifier wraps an identifier in double quotes. Names produced here are
// already lowercase hex with a fixed prefix, so this only guards the statement
// text against a future change.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// TableExists reports whether a relation is visible to the connection, resolved
// through the connection's search path.
func TableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		return false, fmt.Errorf("check relation %s: %w", name, err)
	}
	return exists, nil
}

// schemas lists the harness-owned schemas on the maintenance database. It is
// used by the harness's own tests to prove a schema was dropped.
func schemas(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT nspname FROM pg_namespace WHERE nspname LIKE $1", schemaPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("list test schemas: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan test schema name: %w", err)
		}
		found = append(found, name)
	}
	return found, rows.Err()
}
