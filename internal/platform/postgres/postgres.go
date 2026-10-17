// Package postgres opens and bounds the PostgreSQL connection pool.
//
// The pool is the only place GitNest talks to the database, so its limits are
// explicit rather than left to the driver: a saturated pool must fail a request
// within a bounded wait instead of queueing without limit. The budget values
// come from docs/architecture/performance.md section 12 and are startup
// configuration.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	// The pgx driver registers itself as "pgx" for database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// ErrNotReady reports that the database did not answer a readiness check. It
// carries no further detail on purpose: the readiness endpoint is
// unauthenticated, so it must not disclose a connection string, a host, or a
// database error. Operators get the detail from the startup log instead.
var ErrNotReady = errors.New("database is not ready")

// Settings bounds one process's connection pool.
//
// Every field is required: start from DefaultSettings and override what you
// need. Zero fields are not treated as "use the default", because a zero is
// also a meaningful choice for some of them (no idle connections) and silently
// filling one field can produce a combination the pool cannot honour — an idle
// bound larger than the open bound.
type Settings struct {
	// MaxOpenConns is the hard ceiling on open connections. It is per process,
	// so the web and worker run modes size it differently.
	MaxOpenConns int
	// MaxIdleConns is how many idle connections are kept for reuse.
	MaxIdleConns int
	// ConnMaxLifetime retires a connection after this long, which keeps a
	// long-lived process from holding a connection through a server restart or
	// a failover.
	ConnMaxLifetime time.Duration
	// ConnMaxIdleTime closes a connection that has been idle this long.
	ConnMaxIdleTime time.Duration
	// AcquireTimeout bounds how long a caller waits for a connection, and how
	// long a readiness check may take.
	AcquireTimeout time.Duration
}

// DefaultSettings returns the web-process pool budget from
// docs/architecture/performance.md. The worker run mode passes a smaller
// MaxOpenConns because it must not crowd out request handling.
func DefaultSettings() Settings {
	return Settings{
		MaxOpenConns:    20,
		MaxIdleConns:    5,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
		AcquireTimeout:  2 * time.Second,
	}
}

// Validate reports whether the settings are internally consistent.
func (s Settings) Validate() error {
	if s.MaxOpenConns < 1 {
		return fmt.Errorf("maximum open connections must be at least 1; got %d", s.MaxOpenConns)
	}
	if s.MaxIdleConns < 0 {
		return fmt.Errorf("maximum idle connections must not be negative; got %d", s.MaxIdleConns)
	}
	if s.MaxIdleConns > s.MaxOpenConns {
		return fmt.Errorf("maximum idle connections (%d) must not exceed maximum open connections (%d)",
			s.MaxIdleConns, s.MaxOpenConns)
	}
	for name, value := range map[string]time.Duration{
		"connection maximum lifetime":  s.ConnMaxLifetime,
		"connection maximum idle time": s.ConnMaxIdleTime,
		"acquire timeout":              s.AcquireTimeout,
	} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive; got %s", name, value)
		}
	}
	return nil
}

// Client owns the connection pool.
type Client struct {
	DB       *sql.DB
	settings Settings
}

// Settings returns the pool bounds this client was opened with.
func (c *Client) Settings() Settings { return c.settings }

// poolConfigurer is the part of *sql.DB the pool bounds are applied to. It is
// an interface so a test can assert exactly which bounds are set.
type poolConfigurer interface {
	SetMaxOpenConns(int)
	SetMaxIdleConns(int)
	SetConnMaxLifetime(time.Duration)
	SetConnMaxIdleTime(time.Duration)
}

// applySettings applies every bound explicitly. None of them is left to the
// driver default.
func applySettings(db poolConfigurer, settings Settings) {
	db.SetMaxOpenConns(settings.MaxOpenConns)
	db.SetMaxIdleConns(settings.MaxIdleConns)
	db.SetConnMaxLifetime(settings.ConnMaxLifetime)
	db.SetConnMaxIdleTime(settings.ConnMaxIdleTime)
}

// Open validates the settings, opens the pool, and proves the database answers
// before returning. A connectivity failure is bounded by the context and by the
// acquire timeout, and the returned error names the target without ever
// including the password.
func Open(ctx context.Context, dsn string, settings Settings) (*Client, error) {
	if err := settings.Validate(); err != nil {
		return nil, fmt.Errorf("invalid database settings: %w", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %s", safeTarget(dsn), redact(err.Error(), dsn))
	}
	applySettings(db, settings)

	pingCtx, cancel := context.WithTimeout(ctx, settings.AcquireTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to %s: %s", safeTarget(dsn), redact(err.Error(), dsn))
	}
	return &Client{DB: db, settings: settings}, nil
}

// Ready reports whether the database answers. Every failure is reported as
// ErrNotReady so the readiness endpoint cannot leak connection details.
func (c *Client) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.settings.AcquireTimeout)
	defer cancel()
	if err := c.DB.PingContext(ctx); err != nil {
		return ErrNotReady
	}
	return nil
}

// Close releases the pool.
func (c *Client) Close() error { return c.DB.Close() }

// safeTarget describes the connection target for an operator-facing message:
// host, port, and database name, and never the user or the password.
func safeTarget(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Host == "" {
		return "the configured database"
	}
	target := parsed.Host
	if name := strings.Trim(parsed.Path, "/"); name != "" {
		target += "/" + name
	}
	return target
}

// redact removes every secret the DSN carries from a message, so a driver error
// that echoes the connection string cannot leak the password.
func redact(message, dsn string) string {
	for _, secret := range secrets(dsn) {
		message = strings.ReplaceAll(message, secret, "REDACTED")
	}
	return message
}

// secrets lists the values in a DSN that must never appear in a message.
func secrets(dsn string) []string {
	var found []string
	if parsed, err := url.Parse(dsn); err == nil && parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			found = append(found, password)
		}
	}
	// A keyword/value DSN, for example "host=db password=secret", is not a URL,
	// so scan it for the password field directly.
	for _, field := range strings.Fields(dsn) {
		if name, value, ok := strings.Cut(field, "="); ok && strings.EqualFold(name, "password") && value != "" {
			found = append(found, value)
		}
	}
	return found
}
