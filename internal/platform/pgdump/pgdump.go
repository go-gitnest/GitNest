// Package pgdump captures and restores PostgreSQL through `pg_dump` and
// `pg_restore`.
//
// It is the production half of backup.Database. The split exists so the recovery
// set's own logic — the manifest, verification, the storage copy — is testable
// without a database, and so the process invocation lives in the platform layer
// beside the Git runner, which is where this codebase keeps adapters to external
// programs.
//
// # Credentials never reach the command line
//
// docs/architecture/administration.md states the rule: "No command accepts a
// password, token, or secret as a command-line argument, because arguments are
// visible in the process table and shell history." A connection string on an
// argument vector would violate that on every backup. So the configured DSN is
// parsed here and its parts are passed through the environment — `PGHOST`,
// `PGPORT`, `PGUSER`, `PGDATABASE`, `PGPASSWORD` — which is exactly what libpq reads.
//
// # No shell
//
// Both programs are invoked with an argument vector on an `exec.Cmd` and a built
// environment, with no `sh -c` anywhere. A DSN is operator-supplied configuration
// rather than request data, but the rule from threat model T-A1 is that a client
// value never reaches a shell, and the same discipline is applied to configuration
// for the same reason: one escape hatch in a process that also serves requests is
// one too many.
package pgdump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/backup"
)

// Defaults for the invocations.
const (
	// DefaultTimeout bounds one dump or restore. A restore of a large database is
	// slow, so the bound is generous; it exists so a hung client cannot hold an
	// operator's terminal forever.
	DefaultTimeout = 60 * time.Minute
	// databaseURLScheme is the DSN scheme this adapter accepts.
	databaseURLScheme = "postgres"
	// databaseURLSSLScheme is the alternative spelling.
	databaseURLSSLScheme = "postgresql"
)

// Errors this package reports.
var (
	// ErrBinaryNotFound reports a missing `pg_dump` or `pg_restore`.
	ErrBinaryNotFound = errors.New("the PostgreSQL client program is not on PATH")
	// ErrInvalidDSN reports a connection string this adapter cannot use.
	ErrInvalidDSN = errors.New("the database connection string is not usable")
)

// Settings configure the adapter.
type Settings struct {
	// DSN is the connection string, as internal/config supplies it.
	DSN string
	// DumpBinary is the `pg_dump` executable. Empty means "pg_dump" on PATH.
	DumpBinary string
	// RestoreBinary is the `pg_restore` executable. Empty means "pg_restore".
	RestoreBinary string
	// Timeout bounds one invocation.
	Timeout time.Duration
	// Log receives the operator-facing line for each invocation. It may be nil.
	Log func(string, ...any)
}

// Validate checks the settings before the first use, so a bad DSN fails at startup
// rather than at the moment an operator is trying to take a backup.
func (s Settings) Validate() error {
	if _, err := parseDSN(s.DSN); err != nil {
		return err
	}
	if s.Timeout < 0 {
		return errors.New("the timeout cannot be negative")
	}
	return nil
}

// Adapter is the backup.Database implementation.
type Adapter struct {
	settings Settings
}

// New returns an adapter.
func New(settings Settings) (*Adapter, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if settings.DumpBinary == "" {
		settings.DumpBinary = "pg_dump"
	}
	if settings.RestoreBinary == "" {
		settings.RestoreBinary = "pg_restore"
	}
	if settings.Timeout == 0 {
		settings.Timeout = DefaultTimeout
	}
	return &Adapter{settings: settings}, nil
}

// connection is the parts of a DSN, in the form libpq's environment variables take.
type connection struct {
	host     string
	port     string
	user     string
	password string
	database string
	options  []string
}

// parseDSN reads a PostgreSQL connection string.
//
// Both the URL form (`postgres://user:pass@host:port/db?sslmode=...`) and the
// keyword form (`host=... user=...`) are accepted, because an operator may have
// either in their environment and refusing one would only push them to work around
// this code. The password is extracted so it can be moved into the environment and
// kept off the command line.
func parseDSN(dsn string) (connection, error) {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return connection{}, fmt.Errorf("%w: it is empty", ErrInvalidDSN)
	}

	if strings.Contains(trimmed, "://") {
		return parseURLDSN(trimmed)
	}
	return parseKeywordDSN(trimmed)
}

// parseURLDSN reads the URL form.
func parseURLDSN(dsn string) (connection, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return connection{}, fmt.Errorf("%w: %v", ErrInvalidDSN, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != databaseURLScheme && scheme != databaseURLSSLScheme {
		return connection{}, fmt.Errorf("%w: the scheme %q is not PostgreSQL", ErrInvalidDSN, parsed.Scheme)
	}

	// The variable is named `found`, not `connection`, so the type stays reachable
	// in this function's return statements.
	found := connection{
		host: parsed.Hostname(),
		port: parsed.Port(),
	}
	if parsed.User != nil {
		found.user = parsed.User.Username()
		if password, ok := parsed.User.Password(); ok {
			found.password = password
		}
	}
	found.database = strings.TrimPrefix(parsed.Path, "/")

	// The query carries the libpq parameters the URL form cannot express. They are
	// passed through as `PG*` variables, uppercased, which is the documented
	// mapping.
	for key, values := range parsed.Query() {
		if len(values) == 0 {
			continue
		}
		found.options = append(found.options, "PG"+strings.ToUpper(key)+"="+values[0])
	}
	sortStrings(found.options)

	if found.database == "" {
		return connection{}, fmt.Errorf("%w: it names no database", ErrInvalidDSN)
	}
	return found, nil
}

// parseKeywordDSN reads the keyword form, which is space-separated `key=value`
// pairs with quoting.
func parseKeywordDSN(dsn string) (connection, error) {
	var found connection

	for _, field := range splitKeywords(dsn) {
		key, value, haveValue := strings.Cut(field, "=")
		if !haveValue {
			return connection{}, fmt.Errorf("%w: %q is not a key=value pair", ErrInvalidDSN, field)
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "host":
			found.host = value
		case "port":
			found.port = value
		case "user":
			found.user = value
		case "password":
			found.password = value
		case "dbname", "database":
			found.database = value
		case "sslmode", "connect_timeout", "application_name", "options":
			found.options = append(found.options, "PG"+strings.ToUpper(strings.TrimSpace(key))+"="+value)
		default:
			// An unrecognised keyword is refused rather than ignored: silently
			// dropping a parameter an operator wrote could connect to the wrong
			// database, and a backup of the wrong database is worse than no backup.
			return connection{}, fmt.Errorf("%w: the parameter %q is not recognised", ErrInvalidDSN, key)
		}
	}
	if found.database == "" {
		return connection{}, fmt.Errorf("%w: it names no database", ErrInvalidDSN)
	}
	sortStrings(found.options)
	return found, nil
}

// splitKeywords splits the keyword form on unquoted whitespace, honouring single
// quotes, which is the grammar libpq documents.
func splitKeywords(dsn string) []string {
	var fields []string
	var current strings.Builder
	var quoted bool

	for _, r := range dsn {
		switch {
		case r == '\'':
			quoted = !quoted
		case (r == ' ' || r == '\t' || r == '\n') && !quoted:
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}

// environment builds the process environment for an invocation.
//
// It starts from the OS environment so `PATH` and the Windows system variables
// survive — the programs are found through `PATH`, and a stripped environment breaks
// that on Windows — then sets the connection variables, overwriting anything of the
// same name. Overwriting matters: an inherited `PGDATABASE` pointing somewhere else
// must not decide which database is dumped.
func (a *Adapter) environment(ctx context.Context) ([]string, error) {
	connection, err := parseDSN(a.settings.DSN)
	if err != nil {
		return nil, err
	}

	environment := make([]string, 0, len(os.Environ())+8)
	overridden := map[string]bool{
		"PGHOST": true, "PGPORT": true, "PGUSER": true,
		"PGPASSWORD": true, "PGDATABASE": true,
	}
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if found && overridden[strings.ToUpper(name)] {
			continue
		}
		// A cancellation is the operator's interrupt; the child is expected to see
		// it, so it is passed through rather than swallowed.
		environment = append(environment, entry)
	}

	set := func(name, value string) {
		if value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	set("PGHOST", connection.host)
	set("PGPORT", connection.port)
	set("PGUSER", connection.user)
	set("PGDATABASE", connection.database)
	// The password is the whole point of the environment: it is set unconditionally
	// when present so it never appears in the argument vector.
	set("PGPASSWORD", connection.password)
	environment = append(environment, connection.options...)

	// The dump must not be affected by a locale the operator happens to have set, so
	// its output is deterministic.
	environment = append(environment, "LC_ALL=C", "PGCLIENTENCODING=UTF8")

	// The connection context is checked here so a cancelled run does not start a
	// process it will immediately kill.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return environment, nil
}

// Dump writes the database to w and reports what the set must record.
//
// `pg_dump` is invoked in the custom format (`-Fc`), which is compressed and is what
// `pg_restore` reads. The plain format would be larger and harder to restore
// selectively when a later phase wants that.
func (a *Adapter) Dump(ctx context.Context, w io.Writer) (backup.DatabaseInfo, error) {
	environment, err := a.environment(ctx)
	if err != nil {
		return backup.DatabaseInfo{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, a.settings.Timeout)
	defer cancel()

	command := exec.CommandContext(runCtx, a.settings.DumpBinary,
		"--format=custom",
		"--no-owner",
		"--no-privileges",
		// Every schema is dumped rather than only `public`. Naming one would silently
		// produce an empty set on a database whose objects live elsewhere, and an
		// empty dump is the failure that looks like success.
	)
	command.Env = environment
	command.Stdin = nil
	command.Stdout = w
	// stderr carries the program's own message, which is what an operator needs to
	// see. It is captured rather than inherited so a failure can report it.
	var diagnostics diagnosticsBuffer
	command.Stderr = &diagnostics

	if err := command.Run(); err != nil {
		return backup.DatabaseInfo{}, a.describe("pg_dump", err, diagnostics.String())
	}

	info, err := a.inspect(ctx, environment)
	if err != nil {
		return backup.DatabaseInfo{}, err
	}
	return info, nil
}

// Restore replaces the database from r.
//
// `--clean --if-exists` makes the restore idempotent: restoring the same set twice
// succeeds rather than failing on an object that already exists. That is what lets an
// interrupted restore be re-run, which the architecture document requires of a
// cross-boundary operation ("any cross-boundary operation needs an idempotent repair
// path").
func (a *Adapter) Restore(ctx context.Context, r io.Reader) error {
	environment, err := a.environment(ctx)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, a.settings.Timeout)
	defer cancel()

	command := exec.CommandContext(runCtx, a.settings.RestoreBinary,
		"--clean", "--if-exists",
		"--no-owner", "--no-privileges",
		"--exit-on-error",
	)
	command.Env = environment
	command.Stdin = r
	command.Stdout = io.Discard
	var diagnostics diagnosticsBuffer
	command.Stderr = &diagnostics

	if err := command.Run(); err != nil {
		return a.describe("pg_restore", err, diagnostics.String())
	}
	return nil
}

// describe turns a failed invocation into an error an operator can act on.
//
// The program's own diagnostics are included because they name the actual problem —
// a version mismatch, a refused connection, a permission — and an operator staring at
// "exit status 1" has nothing to work with.
func (a *Adapter) describe(program string, err error, diagnostics string) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("%w: %s", ErrBinaryNotFound, program)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s timed out after %s", program, a.settings.Timeout)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s was interrupted", program)
	}

	detail := strings.TrimSpace(diagnostics)
	if detail == "" {
		return fmt.Errorf("%s failed: %w", program, err)
	}
	return fmt.Errorf("%s failed: %w: %s", program, err, detail)
}

// inspect reads what the manifest must record: the schema version and the
// repository rows.
//
// It is a second connection rather than an addition to the dump, because the
// manifest's contents are not part of the dump and `pg_dump` cannot emit them. The
// queries are read-only, which matters: a capture must not write to the database it
// is capturing.
func (a *Adapter) inspect(ctx context.Context, environment []string) (backup.DatabaseInfo, error) {
	var info backup.DatabaseInfo

	version, err := a.scalar(ctx, environment,
		`SELECT name FROM schema_migrations ORDER BY name DESC LIMIT 1`)
	if err != nil {
		return backup.DatabaseInfo{}, err
	}
	info.SchemaVersion = version

	rows, err := a.query(ctx, environment,
		`SELECT id, storage_id, name FROM repositories ORDER BY storage_id`)
	if err != nil {
		return backup.DatabaseInfo{}, err
	}
	for _, row := range rows {
		if len(row) != 3 {
			return backup.DatabaseInfo{}, fmt.Errorf("a repository row has %d columns, want 3", len(row))
		}
		info.Repositories = append(info.Repositories, backup.RepositoryRef{
			ID: row[0], StorageID: row[1], Name: row[2],
		})
	}
	return info, nil
}

// scalar runs a query and returns its single value.
func (a *Adapter) scalar(ctx context.Context, environment []string, query string) (string, error) {
	rows, err := a.query(ctx, environment, query)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return "", nil
	}
	return rows[0][0], nil
}

// query runs a read-only query through `psql` and returns the rows.
//
// `psql` rather than a database driver: this adapter already depends on the
// PostgreSQL client programs being present, and adding a driver here would mean two
// ways to reach the same database in the backup path — one of which would not be
// exercised by the operator's own tooling. The output is asked for in unaligned,
// unquoted, field-separated form so it can be parsed without a table formatter.
func (a *Adapter) query(ctx context.Context, environment []string, query string) ([][]string, error) {
	runCtx, cancel := context.WithTimeout(ctx, a.settings.Timeout)
	defer cancel()

	// A NUL separator, because a repository name cannot contain one and a tab or
	// pipe could appear in a name.
	const fieldSeparator = "\x1f"
	const recordSeparator = "\x1e"

	command := exec.CommandContext(runCtx, "psql",
		"--no-psqlrc",
		"--quiet",
		"--tuples-only",
		"--no-align",
		"--field-separator", fieldSeparator,
		"--record-separator", recordSeparator,
		"--command", query,
	)
	command.Env = environment

	var out, diagnostics diagnosticsBuffer
	command.Stdout = &out
	command.Stderr = &diagnostics

	if err := command.Run(); err != nil {
		return nil, a.describe("psql", err, diagnostics.String())
	}

	text := strings.TrimRight(out.String(), recordSeparator+"\n\r ")
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}

	var rows [][]string
	for _, record := range strings.Split(text, recordSeparator) {
		if strings.TrimSpace(record) == "" {
			continue
		}
		rows = append(rows, strings.Split(record, fieldSeparator))
	}
	return rows, nil
}

// diagnosticsBuffer collects a child's stderr without a pipe.
//
// `exec.Cmd` with a pipe would need a goroutine `Wait` also waits on, which is the
// shape that made the Git runner's suite hang for four minutes before it was
// replaced. A buffer has none of that.
type diagnosticsBuffer struct {
	builder strings.Builder
}

func (b *diagnosticsBuffer) Write(p []byte) (int, error) {
	// The buffer is bounded: a program that fails by looping would otherwise fill
	// memory before it is reported.
	const limit = 8 << 10
	if b.builder.Len() < limit {
		remaining := limit - b.builder.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.builder.Write(p)
	}
	return len(p), nil
}

func (b *diagnosticsBuffer) String() string { return b.builder.String() }

// AsEnvironment renders a DSN as the environment variables libpq reads.
//
// It is exported because the operator documentation shows the mapping, and an
// operator debugging a connection is better served by seeing what the process will
// receive than by inferring it. **The password is deliberately replaced rather than
// returned**, so this cannot be pasted into a log.
func AsEnvironment(dsn string) ([]string, error) {
	connection, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	variables := []string{
		"PGHOST=" + connection.host,
		"PGPORT=" + connection.port,
		"PGUSER=" + connection.user,
		"PGDATABASE=" + connection.database,
	}
	if connection.password != "" {
		variables = append(variables, "PGPASSWORD=<redacted>")
	}
	variables = append(variables, connection.options...)

	// The list is sorted so a test's assertion is stable, and so the redacted entry
	// cannot be told apart from the others by position.
	sortStrings(variables)
	return variables, nil
}

func sortStrings(values []string) {
	// A tiny insertion sort: the slices here hold fewer than ten entries, and this
	// keeps the package free of an import for a case that never sees large input.
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
