package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// migrationLockKey is the advisory lock that serializes migration runs, so two
// processes started at once cannot apply the same migration twice. The value is
// arbitrary but must not be reused by another GitNest lock.
const migrationLockKey int64 = 0x6769746e65737402

// NoTransactionMarker opts a migration out of the transaction that normally
// wraps the file and its ledger row. It must appear in the leading comment
// block, before the first statement, because statement-level output cannot be
// parsed for it reliably.
const NoTransactionMarker = "-- gitnest:no-transaction"

// migrationNamePattern is the required file naming: a four-digit zero-padded
// sequence so lexical order is application order, a snake_case description, and
// the .up.sql suffix.
var migrationNamePattern = regexp.MustCompile(`^\d{4}_[a-z0-9]+(_[a-z0-9]+)*\.up\.sql$`)

// createLedgerSQL creates the migration ledger. The runner owns this table
// rather than a migration, because the first migration needs somewhere to be
// recorded.
const createLedgerSQL = `
	CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		checksum   TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`

// Migration is one loaded migration file.
type Migration struct {
	// Name is the file name, which is also the ledger key.
	Name string
	// SQL is the file contents.
	SQL string
	// Checksum is the SHA-256 of the contents as applied. It is recorded so an
	// edited migration is detected instead of silently skipped.
	Checksum string
	// NoTransaction reports that the file opted out of the wrapping transaction.
	NoTransaction bool
}

// Report describes what a migration run did.
type Report struct {
	// Applied lists the migrations this run executed, in order.
	Applied []string
	// Skipped lists the migrations already recorded in the ledger.
	Skipped []string
}

// Load reads, validates, and orders the migrations in files. An empty set is
// valid: a build with no migrations simply has nothing to apply.
//
// Every file whose name ends in .sql must be a valid migration name. A stray
// file is reported rather than ignored, because a mistyped suffix or sequence
// would otherwise be silently skipped on a case-insensitive filesystem.
func Load(files fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".sql") {
			continue
		}
		if !migrationNamePattern.MatchString(name) {
			return nil, fmt.Errorf("migration %q must be named NNNN_short_description.up.sql", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	loaded := make([]Migration, 0, len(names))
	for _, name := range names {
		content, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(content)
		loaded = append(loaded, Migration{
			Name:          name,
			SQL:           string(content),
			Checksum:      hex.EncodeToString(sum[:]),
			NoTransaction: usesNoTransactionMarker(string(content)),
		})
	}
	return loaded, nil
}

// usesNoTransactionMarker reports whether the marker appears in the leading
// comment block. A marker after the first statement does not count, because by
// then the decision has already been made.
func usesNoTransactionMarker(sql string) bool {
	for _, raw := range strings.Split(sql, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "--") {
			return false
		}
		if line == NoTransactionMarker {
			return true
		}
	}
	return false
}

// Migrate applies every pending migration in name order and returns what it
// did. It is safe to run concurrently and safe to run repeatedly.
//
// The whole run holds a session-scoped advisory lock, so a second runner waits
// rather than applying the same migration twice; each migration and its ledger
// row share one transaction, so a failure leaves neither behind. A migration
// that opted out of the transaction is executed on its own and recorded
// immediately afterwards.
func (c *Client) Migrate(ctx context.Context, files fs.FS) (report Report, err error) {
	migrations, err := Load(files)
	if err != nil {
		return Report{}, err
	}

	// One pinned connection holds the lock for the whole run, so the lock
	// cannot be released by a pool connection swap and the run cannot deadlock
	// against itself waiting for a second connection.
	conn, err := c.DB.Conn(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return Report{}, fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// The lock must be released even when the caller cancelled the context,
		// otherwise the pinned connection keeps it until it is closed.
		if _, unlockErr := conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("release migration lock: %w", unlockErr)
		}
	}()

	if _, err := conn.ExecContext(ctx, createLedgerSQL); err != nil {
		return Report{}, fmt.Errorf("create migration ledger: %w", err)
	}

	for _, migration := range migrations {
		recorded, found, err := ledgerChecksum(ctx, conn, migration.Name)
		if err != nil {
			return report, err
		}
		switch {
		case !found:
			if err := applyMigration(ctx, conn, migration); err != nil {
				return report, err
			}
			report.Applied = append(report.Applied, migration.Name)
		case recorded != migration.Checksum:
			return report, fmt.Errorf(
				"migration %s changed after it was applied (recorded %s, now %s); add a new migration instead of editing an applied one",
				migration.Name, recorded, migration.Checksum)
		default:
			report.Skipped = append(report.Skipped, migration.Name)
		}
	}
	return report, nil
}

// ledgerChecksum returns the recorded checksum for a migration, if any.
func ledgerChecksum(ctx context.Context, conn *sql.Conn, name string) (string, bool, error) {
	var checksum string
	err := conn.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE name = $1", name).Scan(&checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read migration ledger for %s: %w", name, err)
	}
	return checksum, true, nil
}

// applyMigration applies one migration and records it.
func applyMigration(ctx context.Context, conn *sql.Conn, migration Migration) error {
	if migration.NoTransaction {
		if _, err := conn.ExecContext(ctx, migration.SQL); err != nil {
			return fmt.Errorf("apply migration %s: %w", migration.Name, err)
		}
		// A separate statement on purpose: the file could not be wrapped in a
		// transaction, so this migration must be written to be re-runnable.
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)", migration.Name, migration.Checksum); err != nil {
			return fmt.Errorf("record migration %s: %w", migration.Name, err)
		}
		return nil
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", migration.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("apply migration %s: %w", migration.Name, err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)", migration.Name, migration.Checksum); err != nil {
		return fmt.Errorf("record migration %s: %w", migration.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", migration.Name, err)
	}
	return nil
}
