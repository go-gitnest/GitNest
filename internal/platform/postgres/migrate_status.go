package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// The migration preflight.
//
// docs/architecture/packaging.md section 11 step 4 requires it: "`gitnest migrate
// --status` reports the applied and expected app versions; `gitnest migrate
// --dry-run` lists the migrations that would be applied and exits non-zero when the
// database is newer than the binary. The preflight is run against a copy of the
// production database when the release adds data-rewriting migrations."
//
// Two properties follow from that and shape this file:
//
//   - **It changes nothing.** An operator runs it against production, and often
//     against a copy they intend to keep as-is. It never creates the ledger, never
//     takes the migration lock, and never applies anything. The ledger's existence
//     is probed rather than ensured.
//   - **A database ahead of the binary is a failure, not a warning.** That is the
//     state where a rollback happened to the binary but not to the database, and
//     continuing would run code against a schema it was not written for. Step 4 says
//     the preflight "exits non-zero" for it.

// ErrDatabaseAhead reports a database whose ledger holds migrations this build does
// not carry.
//
// It is the one condition the preflight treats as a hard failure, because it means
// the binary is older than the schema it is about to serve.
var ErrDatabaseAhead = errors.New("the database is ahead of this binary")

// ErrMigrationEdited reports an applied migration whose file has changed.
//
// Editing an applied migration is forbidden — the rule is in the migration runner's
// package documentation — and the preflight is where an operator should learn it,
// rather than when a deployment fails halfway.
var ErrMigrationEdited = errors.New("an applied migration was edited")

// Status is the difference between a build's migrations and a database's ledger.
type Status struct {
	// Applied are the migrations the ledger records, in order.
	Applied []string
	// Pending are the migrations this build carries that the ledger does not.
	Pending []string
	// Edited are migrations recorded in the ledger whose file no longer matches the
	// checksum recorded with them.
	Edited []string
	// Unknown are migrations the ledger records that this build does not carry. A
	// non-empty value means the database is ahead of the binary.
	Unknown []string
	// LedgerExists reports whether the ledger table was present. A database that has
	// never been migrated has none, which is a normal state rather than an error.
	LedgerExists bool
}

// UpToDate reports whether this build and the database agree exactly.
func (s Status) UpToDate() bool {
	return len(s.Pending) == 0 && len(s.Edited) == 0 && len(s.Unknown) == 0
}

// Ahead reports whether the database holds migrations this build does not.
func (s Status) Ahead() bool { return len(s.Unknown) > 0 }

// Describe renders the status as the lines an operator reads.
//
// It is a method rather than a `String` so the caller chooses where it goes, and so
// a test can assert the text without capturing a stream.
func (s Status) Describe() []string {
	lines := []string{
		fmt.Sprintf("  applied            %d", len(s.Applied)),
		fmt.Sprintf("  pending            %d", len(s.Pending)),
	}
	for _, name := range s.Pending {
		lines = append(lines, "    would apply      "+name)
	}
	if len(s.Edited) > 0 {
		lines = append(lines, fmt.Sprintf("  edited             %d", len(s.Edited)))
		for _, name := range s.Edited {
			lines = append(lines, "    changed          "+name)
		}
	}
	if len(s.Unknown) > 0 {
		lines = append(lines, fmt.Sprintf("  unknown            %d", len(s.Unknown)))
		for _, name := range s.Unknown {
			lines = append(lines, "    not in this build "+name)
		}
	}
	if !s.LedgerExists {
		lines = append(lines, "  the database has never been migrated")
	}
	return lines
}

// Check compares a build's migrations with a database's ledger, changing nothing.
//
// It reads the ledger directly rather than through the migration runner, because the
// runner takes the migration lock and creates the ledger — both of which are writes.
// The queries here are read-only, so this is safe against production and against a
// copy an operator intends to keep.
//
// It returns the status alongside the error rather than instead of it, so a caller can
// report what it found even when the answer is "this will not work": an operator needs
// the list of unknown migrations, not only the fact that there is one.
func (c *Client) Check(ctx context.Context, files fs.FS) (Status, error) {
	migrations, err := Load(files)
	if err != nil {
		return Status{}, err
	}

	var status Status

	// The ledger is probed, never created. A schema that has never been migrated has
	// no table, and creating one here would turn a read-only preflight into a write.
	var present bool
	if err := c.DB.QueryRowContext(ctx,
		`SELECT to_regclass(current_schema() || '.schema_migrations') IS NOT NULL`).Scan(&present); err != nil {
		return status, fmt.Errorf("look for the migration ledger: %w", err)
	}
	status.LedgerExists = present

	recorded := map[string]string{}
	if present {
		rows, err := c.DB.QueryContext(ctx, `SELECT name, checksum FROM schema_migrations ORDER BY name`)
		if err != nil {
			return status, fmt.Errorf("read the migration ledger: %w", err)
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var name, checksum string
			if err := rows.Scan(&name, &checksum); err != nil {
				return status, fmt.Errorf("read the migration ledger: %w", err)
			}
			recorded[name] = checksum
			status.Applied = append(status.Applied, name)
		}
		if err := rows.Err(); err != nil {
			return status, fmt.Errorf("read the migration ledger: %w", err)
		}
	}

	known := map[string]bool{}
	for _, migration := range migrations {
		known[migration.Name] = true
		checksum, found := recorded[migration.Name]
		switch {
		case !found:
			status.Pending = append(status.Pending, migration.Name)
		case checksum != migration.Checksum:
			// The runner refuses this when it reaches the file. Reporting it here means
			// the operator learns before a deployment is underway, which is the whole
			// point of a preflight.
			status.Edited = append(status.Edited, migration.Name)
		}
	}

	// A ledger entry with no file in this build is the database being ahead. It is the
	// condition step 4 requires a non-zero exit for.
	for _, name := range status.Applied {
		if !known[name] {
			status.Unknown = append(status.Unknown, name)
		}
	}

	switch {
	case len(status.Edited) > 0:
		return status, fmt.Errorf("%w: %s", ErrMigrationEdited, strings.Join(status.Edited, ", "))
	case len(status.Unknown) > 0:
		return status, fmt.Errorf("%w: the ledger records %s",
			ErrDatabaseAhead, strings.Join(status.Unknown, ", "))
	}
	return status, nil
}

// AppliedVersion reports the newest migration the ledger holds, and whether the
// ledger exists at all.
//
// It exists for the operator-facing build identity: `gitnest version` and the release
// notes both want "which schema is this database at", and the answer has to come from
// the database rather than from the binary.
func (c *Client) AppliedVersion(ctx context.Context) (string, bool, error) {
	var present bool
	if err := c.DB.QueryRowContext(ctx,
		`SELECT to_regclass(current_schema() || '.schema_migrations') IS NOT NULL`).Scan(&present); err != nil {
		return "", false, fmt.Errorf("look for the migration ledger: %w", err)
	}
	if !present {
		return "", false, nil
	}

	var name string
	err := c.DB.QueryRowContext(ctx,
		`SELECT name FROM schema_migrations ORDER BY name DESC LIMIT 1`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		// The table exists and is empty, which is a real state: a migrate run that
		// created the ledger and applied nothing.
		return "", true, nil
	}
	if err != nil {
		return "", true, fmt.Errorf("read the newest applied migration: %w", err)
	}
	return name, true, nil
}
