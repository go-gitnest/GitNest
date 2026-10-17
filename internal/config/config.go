// Package config loads and validates the startup configuration of a GitNest
// instance.
//
// Startup configuration is the first of the three configuration layers
// described in docs/architecture/configuration.md: the values that define the
// process and the instance's identity. They are read once, validated as a
// whole, and never changed at runtime. Instance policy lives in the database
// and is deliberately absent here.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variables that configure the process. They are exported so tests,
// documentation, and operator tooling refer to one spelling.
const (
	EnvHost              = "GITNEST_HOST"
	EnvPort              = "GITNEST_PORT"
	EnvPublicURL         = "GITNEST_PUBLIC_URL"
	EnvDatabaseURL       = "GITNEST_DATABASE_URL"
	EnvDataDir           = "GITNEST_DATA_DIR"
	EnvRepoRoot          = "GITNEST_REPO_ROOT"
	EnvTmpRoot           = "GITNEST_TMP_ROOT"
	EnvLogLevel          = "GITNEST_LOG_LEVEL"
	EnvSSHListen         = "GITNEST_SSH_LISTEN"
	EnvReadHeaderTimeout = "GITNEST_READ_HEADER_TIMEOUT"
	EnvShutdownTimeout   = "GITNEST_SHUTDOWN_TIMEOUT"

	// Connection-pool budget, per docs/architecture/performance.md section 12.
	// These are startup configuration because the sizing differs per run mode:
	// the web process gets a larger pool than the worker.
	EnvDBMaxOpenConns    = "GITNEST_DB_MAX_OPEN_CONNS"
	EnvDBMaxIdleConns    = "GITNEST_DB_MAX_IDLE_CONNS"
	EnvDBConnMaxLifetime = "GITNEST_DB_CONN_MAX_LIFETIME"
	EnvDBConnMaxIdleTime = "GITNEST_DB_CONN_MAX_IDLE_TIME"
	EnvDBAcquireTimeout  = "GITNEST_DB_ACQUIRE_TIMEOUT"
)

// Defaults. Every one of them keeps a single-node development instance working
// with no environment at all.
const (
	defaultHost              = "0.0.0.0"
	defaultPort              = 8080
	defaultPublicURL         = "http://localhost:8080"
	defaultDatabaseURL       = "postgres://gitnest:gitnest@localhost:5432/gitnest?sslmode=disable"
	defaultDataDir           = "./data"
	defaultLogLevel          = LogLevelInfo
	defaultSSHListen         = ":2222"
	defaultReadHeaderTimeout = 5 * time.Second
	defaultShutdownTimeout   = 10 * time.Second

	// Connection-pool defaults for the web process (DB-POOL-WEB, DB-IDLE,
	// DB-LIFETIME, DB-IDLE-TIME, DB-ACQUIRE).
	defaultDBMaxOpenConns    = 20
	defaultDBMaxIdleConns    = 5
	defaultDBConnMaxLifetime = 30 * time.Minute
	defaultDBConnMaxIdleTime = 5 * time.Minute
	defaultDBAcquireTimeout  = 2 * time.Second

	repositoriesDirName = "repositories"
	tempDirName         = "tmp"
)

// LogLevel is a supported logging threshold.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// Config is the validated startup configuration.
type Config struct {
	// Host and Port are the HTTP listener address.
	Host string
	Port int

	// PublicURL is the canonical absolute URL of the instance, without a
	// trailing slash. Clone URLs and redirects are built from it; request and
	// forwarded headers are never used instead.
	PublicURL string

	// DatabaseURL is the PostgreSQL connection URL. It contains credentials and
	// must never reach a log, an error message, or a metric label.
	DatabaseURL string

	// DataDir is the writable root for instance data, normally one mounted
	// volume in a container deployment.
	DataDir string
	// RepoRoot holds the bare Git repositories. It defaults to
	// DataDir/repositories.
	RepoRoot string
	// TmpRoot is scratch space for Git operations. It is cleaned on startup and
	// must not contain the repository root or live inside it. It defaults to
	// DataDir/tmp.
	TmpRoot string

	// LogLevel is the minimum level that is logged.
	LogLevel LogLevel
	// SSHListen is the Git-over-SSH listen address, unused until the SSH
	// transport lands.
	SSHListen string
	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers.
	ReadHeaderTimeout time.Duration
	// ShutdownTimeout bounds how long a graceful shutdown may take.
	ShutdownTimeout time.Duration

	// DBMaxOpenConns is the hard ceiling on open connections for this process.
	DBMaxOpenConns int
	// DBMaxIdleConns is how many idle connections are kept for reuse.
	DBMaxIdleConns int
	// DBConnMaxLifetime retires a connection after this long.
	DBConnMaxLifetime time.Duration
	// DBConnMaxIdleTime closes a connection that has been idle this long.
	DBConnMaxIdleTime time.Duration
	// DBAcquireTimeout bounds how long a caller waits for a connection.
	DBAcquireTimeout time.Duration
}

// HTTPAddress returns the host:port the HTTP server binds to.
func (c Config) HTTPAddress() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// Secrets returns the values in this configuration that must never appear in a
// log line, an error message, or a metric label.
//
// The logging layer registers them for redaction at startup, so a connection
// failure that echoes the connection string cannot leak its password.
func (c Config) Secrets() []string {
	parsed, err := url.Parse(c.DatabaseURL)
	if err != nil || parsed.User == nil {
		return nil
	}
	password, ok := parsed.User.Password()
	if !ok || password == "" {
		return nil
	}
	return []string{password}
}

// Load reads the startup configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(os.LookupEnv)
}

// LoadFrom reads the startup configuration from lookup, which reports whether a
// variable is present. It is the pure form of Load: it never touches the
// process environment or any global state, so a test supplies its own variables
// and calling it twice with the same input yields the same result.
//
// A variable that is absent takes its default. A variable that is present but
// blank is an error rather than a silent fallback, because an empty value is
// almost always an accident. Every problem is reported at once, and each names
// the variable it came from.
func LoadFrom(lookup func(name string) (string, bool)) (Config, error) {
	present, errs := read(lookup)
	cfg, buildErrs := build(present)
	errs = append(errs, buildErrs...)
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// read collects the variables that are explicitly set, rejecting blank values.
func read(lookup func(name string) (string, bool)) (map[string]string, []error) {
	names := []string{
		EnvHost, EnvPort, EnvPublicURL, EnvDatabaseURL, EnvDataDir, EnvRepoRoot,
		EnvTmpRoot, EnvLogLevel, EnvSSHListen, EnvReadHeaderTimeout, EnvShutdownTimeout,
		EnvDBMaxOpenConns, EnvDBMaxIdleConns, EnvDBConnMaxLifetime, EnvDBConnMaxIdleTime, EnvDBAcquireTimeout,
	}
	present := make(map[string]string, len(names))
	var errs []error
	for _, name := range names {
		value, ok := lookup(name)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			errs = append(errs, fmt.Errorf("%s must not be empty when it is set", name))
			continue
		}
		present[name] = value
	}
	return present, errs
}

// build applies defaults and validates every value.
func build(present map[string]string) (Config, []error) {
	var errs []error

	cfg := Config{}
	cfg.Host = valueOr(present, EnvHost, defaultHost)
	if strings.ContainsAny(cfg.Host, " \t\r\n") {
		errs = append(errs, fmt.Errorf("%s must not contain whitespace", EnvHost))
	}

	cfg.Port, errs = intValue(present, EnvPort, defaultPort, 1, 65535, errs)

	publicURL, err := publicURLValue(valueOr(present, EnvPublicURL, defaultPublicURL))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.PublicURL = publicURL

	cfg.DatabaseURL = valueOr(present, EnvDatabaseURL, defaultDatabaseURL)
	if err := validateDatabaseURL(cfg.DatabaseURL); err != nil {
		errs = append(errs, err)
	}

	cfg.DataDir = filepath.Clean(valueOr(present, EnvDataDir, defaultDataDir))
	cfg.RepoRoot = filepath.Clean(valueOr(present, EnvRepoRoot, filepath.Join(cfg.DataDir, repositoriesDirName)))
	cfg.TmpRoot = filepath.Clean(valueOr(present, EnvTmpRoot, filepath.Join(cfg.DataDir, tempDirName)))
	errs = append(errs, validateRoots(cfg.RepoRoot, cfg.TmpRoot)...)

	level := LogLevel(strings.ToLower(valueOr(present, EnvLogLevel, string(defaultLogLevel))))
	if !level.Valid() {
		errs = append(errs, fmt.Errorf("%s must be one of %s, %s, %s, %s; got %q",
			EnvLogLevel, LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError, string(level)))
	}
	cfg.LogLevel = level

	cfg.SSHListen = valueOr(present, EnvSSHListen, defaultSSHListen)
	if err := validateListenAddress(EnvSSHListen, cfg.SSHListen); err != nil {
		errs = append(errs, err)
	}

	cfg.ReadHeaderTimeout, errs = durationValue(present, EnvReadHeaderTimeout, defaultReadHeaderTimeout, errs)
	cfg.ShutdownTimeout, errs = durationValue(present, EnvShutdownTimeout, defaultShutdownTimeout, errs)

	cfg.DBMaxOpenConns, errs = intValue(present, EnvDBMaxOpenConns, defaultDBMaxOpenConns, 1, 10000, errs)
	cfg.DBMaxIdleConns, errs = intValue(present, EnvDBMaxIdleConns, defaultDBMaxIdleConns, 0, 10000, errs)
	if cfg.DBMaxIdleConns > cfg.DBMaxOpenConns {
		errs = append(errs, fmt.Errorf("%s (%d) must not exceed %s (%d)",
			EnvDBMaxIdleConns, cfg.DBMaxIdleConns, EnvDBMaxOpenConns, cfg.DBMaxOpenConns))
	}
	cfg.DBConnMaxLifetime, errs = durationValue(present, EnvDBConnMaxLifetime, defaultDBConnMaxLifetime, errs)
	cfg.DBConnMaxIdleTime, errs = durationValue(present, EnvDBConnMaxIdleTime, defaultDBConnMaxIdleTime, errs)
	cfg.DBAcquireTimeout, errs = durationValue(present, EnvDBAcquireTimeout, defaultDBAcquireTimeout, errs)

	return cfg, errs
}

// Valid reports whether the level is one this build understands.
func (l LogLevel) Valid() bool {
	switch l {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		return true
	default:
		return false
	}
}

func valueOr(present map[string]string, name, fallback string) string {
	if value, ok := present[name]; ok {
		return value
	}
	return fallback
}

func intValue(present map[string]string, name string, fallback, min, max int, errs []error) (int, []error) {
	raw, ok := present[name]
	if !ok {
		return fallback, errs
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback, append(errs, fmt.Errorf("%s must be a whole number; got %q", name, raw))
	}
	if value < min || value > max {
		return fallback, append(errs, fmt.Errorf("%s must be between %d and %d; got %d", name, min, max, value))
	}
	return value, errs
}

func durationValue(present map[string]string, name string, fallback time.Duration, errs []error) (time.Duration, []error) {
	raw, ok := present[name]
	if !ok {
		return fallback, errs
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback, append(errs, fmt.Errorf("%s must be a duration such as 5s or 1m; got %q", name, raw))
	}
	if value <= 0 {
		return fallback, append(errs, fmt.Errorf("%s must be positive; got %q", name, raw))
	}
	return value, errs
}

// publicURLValue normalizes and validates the instance's canonical URL.
//
// The value is used to build clone URLs and redirects, and request or forwarded
// headers are never consulted instead, so an unexpected Host header cannot
// change a generated URL. Plain HTTP is accepted only for a loopback host,
// which keeps a development instance convenient without letting a public
// deployment hand out cleartext URLs.
func publicURLValue(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s must be an absolute URL: %w", EnvPublicURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s must use http or https; got %q", EnvPublicURL, parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return "", fmt.Errorf("%s must include a host", EnvPublicURL)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%s must not contain credentials", EnvPublicURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not contain a query or fragment", EnvPublicURL)
	}
	if path := strings.Trim(parsed.Path, "/"); path != "" {
		return "", fmt.Errorf("%s must not contain a path; got %q", EnvPublicURL, parsed.Path)
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("%s must use https for the non-loopback host %q", EnvPublicURL, parsed.Hostname())
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateDatabaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s must be a PostgreSQL URL: %w", EnvDatabaseURL, err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return fmt.Errorf("%s must use the postgres or postgresql scheme; got %q", EnvDatabaseURL, parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("%s must include a host", EnvDatabaseURL)
	}
	if strings.Trim(parsed.Path, "/") == "" {
		return fmt.Errorf("%s must include a database name", EnvDatabaseURL)
	}
	return nil
}

// validateListenAddress accepts a host:port pair where an empty host means
// every interface.
func validateListenAddress(name, raw string) error {
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return fmt.Errorf("%s must be a host:port address such as :2222 or 127.0.0.1:2222; got %q", name, raw)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("%s has a non-numeric port %q", name, port)
	}
	if host != "" && net.ParseIP(host) == nil && strings.ContainsAny(host, " \t") {
		return fmt.Errorf("%s has an invalid host %q", name, host)
	}
	return nil
}

// validateRoots keeps the temporary root and the repository root apart. A
// temporary root that contains the repository root would let startup cleanup
// delete repositories, and the reverse mistake hides repositories inside
// scratch data.
func validateRoots(repoRoot, tmpRoot string) []error {
	if repoRoot == tmpRoot {
		return []error{fmt.Errorf("%s and %s must not be the same directory", EnvRepoRoot, EnvTmpRoot)}
	}
	var errs []error
	if within(tmpRoot, repoRoot) {
		errs = append(errs, fmt.Errorf("%s must not live inside %s", EnvRepoRoot, EnvTmpRoot))
	}
	if within(repoRoot, tmpRoot) {
		errs = append(errs, fmt.Errorf("%s must not live inside %s", EnvTmpRoot, EnvRepoRoot))
	}
	return errs
}

// within reports whether child is strictly inside parent.
func within(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return relative != ".." && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
