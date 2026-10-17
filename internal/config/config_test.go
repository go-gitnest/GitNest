package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// env builds a lookup function from a set of variables.
func env(pairs map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := pairs[name]
		return value, ok
	}
}

// empty is an environment with nothing set.
func empty() func(string) (string, bool) {
	return env(nil)
}

func mustLoad(t *testing.T, pairs map[string]string) Config {
	t.Helper()
	cfg, err := LoadFrom(env(pairs))
	if err != nil {
		t.Fatalf("LoadFrom(%v): %v", pairs, err)
	}
	return cfg
}

func TestDefaultsWithNoEnvironment(t *testing.T) {
	cfg := mustLoad(t, nil)

	if cfg.Host != "0.0.0.0" {
		t.Errorf("Host = %q, want 0.0.0.0", cfg.Host)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.PublicURL != "http://localhost:8080" {
		t.Errorf("PublicURL = %q, want the loopback default", cfg.PublicURL)
	}
	if !strings.HasPrefix(cfg.DatabaseURL, "postgres://") {
		t.Errorf("DatabaseURL = %q, want a postgres URL", cfg.DatabaseURL)
	}
	if cfg.DataDir != "data" {
		t.Errorf("DataDir = %q, want ./data cleaned to data", cfg.DataDir)
	}
	if want := filepath.Join(cfg.DataDir, "repositories"); cfg.RepoRoot != want {
		t.Errorf("RepoRoot = %q, want %q", cfg.RepoRoot, want)
	}
	if want := filepath.Join(cfg.DataDir, "tmp"); cfg.TmpRoot != want {
		t.Errorf("TmpRoot = %q, want %q", cfg.TmpRoot, want)
	}
	if cfg.LogLevel != LogLevelInfo {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.SSHListen != ":2222" {
		t.Errorf("SSHListen = %q, want :2222", cfg.SSHListen)
	}
	if cfg.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 5s", cfg.ReadHeaderTimeout)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 10s", cfg.ShutdownTimeout)
	}
}

func TestOneVariableDoesNotAffectTheOthers(t *testing.T) {
	cfg := mustLoad(t, map[string]string{EnvHost: "127.0.0.1"})

	if cfg.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.Port != 8080 || cfg.LogLevel != LogLevelInfo {
		t.Errorf("setting the host changed unrelated values: port=%d level=%q", cfg.Port, cfg.LogLevel)
	}
}

// TestBlankVariableIsRejected pins the rule that an explicitly empty variable is
// an accident, not a request for the default.
func TestBlankVariableIsRejected(t *testing.T) {
	names := []string{
		EnvHost, EnvPort, EnvPublicURL, EnvDatabaseURL, EnvDataDir, EnvRepoRoot,
		EnvTmpRoot, EnvLogLevel, EnvSSHListen, EnvReadHeaderTimeout, EnvShutdownTimeout,
		EnvDBMaxOpenConns, EnvDBMaxIdleConns, EnvDBConnMaxLifetime, EnvDBConnMaxIdleTime, EnvDBAcquireTimeout,
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{name: "   "}))
			if err == nil {
				t.Fatalf("%s set to whitespace was accepted", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error = %v, want it to name %s", err, name)
			}
		})
	}
}

// TestInvalidValueNamesTheVariable pins the operator requirement: a startup
// failure must say which variable to fix.
func TestInvalidValueNamesTheVariable(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: EnvHost, value: "not a host"},
		{name: EnvPort, value: "70000"},
		{name: EnvPort, value: "http"},
		{name: EnvPublicURL, value: "http://example.com"},
		{name: EnvPublicURL, value: "example.com"},
		{name: EnvDatabaseURL, value: "mysql://user@localhost/db"},
		{name: EnvLogLevel, value: "verbose"},
		{name: EnvSSHListen, value: "2222"},
		{name: EnvReadHeaderTimeout, value: "soon"},
		{name: EnvShutdownTimeout, value: "-1s"},
	} {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{test.name: test.value}))
			if err == nil {
				t.Fatalf("%s=%q was accepted", test.name, test.value)
			}
			if !strings.Contains(err.Error(), test.name) {
				t.Errorf("error = %v, want it to name %s", err, test.name)
			}
		})
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		EnvPort:      "99999",
		EnvLogLevel:  "loud",
		EnvPublicURL: "ftp://example.com",
	}))
	if err == nil {
		t.Fatal("three invalid values were accepted")
	}
	for _, name := range []string{EnvPort, EnvLogLevel, EnvPublicURL} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error = %v, want it to report %s as well", err, name)
		}
	}
}

func TestPublicURLShapes(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "https host", value: "https://git.example.com", want: "https://git.example.com"},
		{name: "trailing slash removed", value: "https://git.example.com/", want: "https://git.example.com"},
		{name: "port kept", value: "https://git.example.com:8443", want: "https://git.example.com:8443"},
		{name: "loopback http", value: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "localhost http", value: "http://localhost:3000", want: "http://localhost:3000"},
		{name: "ipv6 loopback", value: "http://[::1]:8080", want: "http://[::1]:8080"},
		{name: "remote http", value: "http://git.example.com", wantErr: true},
		{name: "missing scheme", value: "git.example.com", wantErr: true},
		{name: "unsupported scheme", value: "ftp://git.example.com", wantErr: true},
		{name: "credentials", value: "https://user:pass@git.example.com", wantErr: true},
		{name: "base path", value: "https://git.example.com/gitnest", wantErr: true},
		{name: "query", value: "https://git.example.com/?x=1", wantErr: true},
		{name: "fragment", value: "https://git.example.com/#top", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := LoadFrom(env(map[string]string{EnvPublicURL: test.value}))
			if test.wantErr {
				if err == nil {
					t.Fatalf("%s = %q was accepted as %q", EnvPublicURL, test.value, cfg.PublicURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s = %q: %v", EnvPublicURL, test.value, err)
			}
			if cfg.PublicURL != test.want {
				t.Fatalf("PublicURL = %q, want %q", cfg.PublicURL, test.want)
			}
		})
	}
}

func TestDatabaseURLShapes(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "postgres scheme", value: "postgres://user:secret@db:5432/gitnest"},
		{name: "postgresql scheme", value: "postgresql://user@db/gitnest?sslmode=require"},
		{name: "ipv6 host", value: "postgres://user@[::1]:5432/gitnest"},
		{name: "wrong scheme", value: "mysql://user@db/gitnest", wantErr: true},
		{name: "no host", value: "postgres:///gitnest", wantErr: true},
		{name: "no database name", value: "postgres://user@db", wantErr: true},
		{name: "not a url", value: "postgres://user@%zz/gitnest", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{EnvDatabaseURL: test.value}))
			if test.wantErr && err == nil {
				t.Fatalf("%s = %q was accepted", EnvDatabaseURL, test.value)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("%s = %q: %v", EnvDatabaseURL, test.value, err)
			}
		})
	}
}

// TestDatabaseErrorHidesThePassword keeps credentials out of startup failures.
func TestDatabaseErrorHidesThePassword(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{EnvDatabaseURL: "mysql://user:sup3rsecret@db/gitnest"}))
	if err == nil {
		t.Fatal("a non-postgres URL was accepted")
	}
	if strings.Contains(err.Error(), "sup3rsecret") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestPortShapes(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    int
		wantErr bool
	}{
		{value: "1", want: 1},
		{value: "65535", want: 65535},
		{value: "0", wantErr: true},
		{value: "-1", wantErr: true},
		{value: "65536", wantErr: true},
		{value: "abc", wantErr: true},
		{value: "80.5", wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			cfg, err := LoadFrom(env(map[string]string{EnvPort: test.value}))
			if test.wantErr {
				if err == nil {
					t.Fatalf("port %q was accepted as %d", test.value, cfg.Port)
				}
				return
			}
			if err != nil {
				t.Fatalf("port %q: %v", test.value, err)
			}
			if cfg.Port != test.want {
				t.Fatalf("Port = %d, want %d", cfg.Port, test.want)
			}
		})
	}
}

func TestDurationShapes(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{value: "250ms", want: 250 * time.Millisecond},
		{value: "5s", want: 5 * time.Second},
		{value: "2m", want: 2 * time.Minute},
		{value: "0s", wantErr: true},
		{value: "-5s", wantErr: true},
		{value: "5", wantErr: true},
		{value: "soon", wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			cfg, err := LoadFrom(env(map[string]string{EnvShutdownTimeout: test.value}))
			if test.wantErr {
				if err == nil {
					t.Fatalf("duration %q was accepted as %s", test.value, cfg.ShutdownTimeout)
				}
				return
			}
			if err != nil {
				t.Fatalf("duration %q: %v", test.value, err)
			}
			if cfg.ShutdownTimeout != test.want {
				t.Fatalf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, test.want)
			}
		})
	}
}

func TestListenAddressShapes(t *testing.T) {
	for _, test := range []struct {
		value   string
		wantErr bool
	}{
		{value: ":2222"},
		{value: "127.0.0.1:2222"},
		{value: "[::1]:2222"},
		{value: "0.0.0.0:22"},
		{value: "2222", wantErr: true},
		{value: "localhost", wantErr: true},
		{value: ":notaport", wantErr: true},
		{value: "host with spaces:22", wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{EnvSSHListen: test.value}))
			if test.wantErr && err == nil {
				t.Fatalf("listen address %q was accepted", test.value)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("listen address %q: %v", test.value, err)
			}
		})
	}
}

func TestLogLevelShapes(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    LogLevel
		wantErr bool
	}{
		{value: "debug", want: LogLevelDebug},
		{value: "info", want: LogLevelInfo},
		{value: "warn", want: LogLevelWarn},
		{value: "error", want: LogLevelError},
		{value: "WARN", want: LogLevelWarn},
		{value: "verbose", wantErr: true},
		{value: "fatal", wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			cfg, err := LoadFrom(env(map[string]string{EnvLogLevel: test.value}))
			if test.wantErr {
				if err == nil {
					t.Fatalf("log level %q was accepted as %q", test.value, cfg.LogLevel)
				}
				return
			}
			if err != nil {
				t.Fatalf("log level %q: %v", test.value, err)
			}
			if cfg.LogLevel != test.want {
				t.Fatalf("LogLevel = %q, want %q", cfg.LogLevel, test.want)
			}
		})
	}
}

// TestStorageRootsAreDerivedFromTheDataDir pins the packaging rule that one
// writable data directory holds repository storage and scratch space.
func TestStorageRootsAreDerivedFromTheDataDir(t *testing.T) {
	const dataDir = "/var/lib/gitnest"
	cfg := mustLoad(t, map[string]string{EnvDataDir: dataDir})

	// Compare cleaned paths so the assertion holds on every platform.
	if want := filepath.Clean(dataDir); cfg.DataDir != want {
		t.Fatalf("DataDir = %q, want %q", cfg.DataDir, want)
	}
	if want := filepath.Join(cfg.DataDir, "repositories"); cfg.RepoRoot != want {
		t.Errorf("RepoRoot = %q, want %q", cfg.RepoRoot, want)
	}
	if want := filepath.Join(cfg.DataDir, "tmp"); cfg.TmpRoot != want {
		t.Errorf("TmpRoot = %q, want %q", cfg.TmpRoot, want)
	}
}

func TestExplicitRootsOverrideTheDerivedDefaults(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		EnvDataDir:  "/var/lib/gitnest",
		EnvRepoRoot: "/srv/git/repositories",
		EnvTmpRoot:  "/var/tmp/gitnest",
	})

	if want := filepath.Clean("/srv/git/repositories"); cfg.RepoRoot != want {
		t.Errorf("RepoRoot = %q, want %q", cfg.RepoRoot, want)
	}
	if want := filepath.Clean("/var/tmp/gitnest"); cfg.TmpRoot != want {
		t.Errorf("TmpRoot = %q, want %q", cfg.TmpRoot, want)
	}
}

// TestScratchSpaceMustNotOverlapRepositoryStorage protects repositories from
// startup cleanup and from being hidden inside scratch data.
func TestScratchSpaceMustNotOverlapRepositoryStorage(t *testing.T) {
	for _, test := range []struct {
		name     string
		repoRoot string
		tmpRoot  string
	}{
		{name: "identical", repoRoot: "/srv/git", tmpRoot: "/srv/git"},
		{name: "scratch inside repositories", repoRoot: "/srv/git", tmpRoot: "/srv/git/tmp"},
		{name: "repositories inside scratch", repoRoot: "/srv/git/repos", tmpRoot: "/srv/git"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{
				EnvRepoRoot: test.repoRoot,
				EnvTmpRoot:  test.tmpRoot,
			}))
			if err == nil {
				t.Fatalf("RepoRoot=%q TmpRoot=%q was accepted", test.repoRoot, test.tmpRoot)
			}
		})
	}
}

func TestSiblingRootsAreAccepted(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		EnvRepoRoot: "/srv/gitnest/repositories",
		EnvTmpRoot:  "/srv/gitnest/tmp",
	})
	if cfg.RepoRoot == cfg.TmpRoot {
		t.Fatal("sibling roots collapsed into one directory")
	}
}

// TestLoadFromIsPure pins the acceptance criterion that reading the
// configuration is a pure function of the environment.
func TestLoadFromIsPure(t *testing.T) {
	pairs := map[string]string{EnvPort: "9090", EnvRepoRoot: "/srv/git", EnvTmpRoot: "/srv/tmp"}

	first, err := LoadFrom(env(pairs))
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := LoadFrom(env(pairs))
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if first != second {
		t.Fatalf("two loads of the same environment differ:\n%+v\n%+v", first, second)
	}

	pairs[EnvPort] = "9091"
	third, err := LoadFrom(env(pairs))
	if err != nil {
		t.Fatalf("third load: %v", err)
	}
	if third == first {
		t.Fatal("changing the environment did not change the result")
	}
	if first.Port != 9090 {
		t.Errorf("the earlier result changed to %d; LoadFrom must not share state", first.Port)
	}
}

func TestHTTPAddress(t *testing.T) {
	for _, test := range []struct {
		host string
		port int
		want string
	}{
		{host: "127.0.0.1", port: 8080, want: "127.0.0.1:8080"},
		{host: "0.0.0.0", port: 80, want: "0.0.0.0:80"},
		{host: "::1", port: 8080, want: "[::1]:8080"},
	} {
		cfg := Config{Host: test.host, Port: test.port}
		if got := cfg.HTTPAddress(); got != test.want {
			t.Errorf("HTTPAddress() = %q, want %q", got, test.want)
		}
	}
}

// TestConnectionPoolDefaultsMatchTheDocumentedBudget pins the pool budget from
// docs/architecture/performance.md section 12.
func TestConnectionPoolDefaultsMatchTheDocumentedBudget(t *testing.T) {
	cfg := mustLoad(t, nil)

	if cfg.DBMaxOpenConns != 20 {
		t.Errorf("DBMaxOpenConns = %d, want 20 (DB-POOL-WEB)", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 5 {
		t.Errorf("DBMaxIdleConns = %d, want 5 (DB-IDLE)", cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetime != 30*time.Minute {
		t.Errorf("DBConnMaxLifetime = %s, want 30m (DB-LIFETIME)", cfg.DBConnMaxLifetime)
	}
	if cfg.DBConnMaxIdleTime != 5*time.Minute {
		t.Errorf("DBConnMaxIdleTime = %s, want 5m (DB-IDLE-TIME)", cfg.DBConnMaxIdleTime)
	}
	if cfg.DBAcquireTimeout != 2*time.Second {
		t.Errorf("DBAcquireTimeout = %s, want 2s (DB-ACQUIRE)", cfg.DBAcquireTimeout)
	}
}

func TestConnectionPoolOverrides(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		EnvDBMaxOpenConns:    "5",
		EnvDBMaxIdleConns:    "2",
		EnvDBConnMaxLifetime: "1h",
		EnvDBConnMaxIdleTime: "10m",
		EnvDBAcquireTimeout:  "500ms",
	})

	if cfg.DBMaxOpenConns != 5 || cfg.DBMaxIdleConns != 2 {
		t.Errorf("open/idle = %d/%d, want 5/2", cfg.DBMaxOpenConns, cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetime != time.Hour || cfg.DBConnMaxIdleTime != 10*time.Minute {
		t.Errorf("lifetime/idleTime = %s/%s, want 1h/10m", cfg.DBConnMaxLifetime, cfg.DBConnMaxIdleTime)
	}
	if cfg.DBAcquireTimeout != 500*time.Millisecond {
		t.Errorf("DBAcquireTimeout = %s, want 500ms", cfg.DBAcquireTimeout)
	}
}

// TestIdleConnectionsMustNotExceedOpenConnections catches a pool configuration
// that would make the driver silently clamp the idle bound.
func TestIdleConnectionsMustNotExceedOpenConnections(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		EnvDBMaxOpenConns: "4",
		EnvDBMaxIdleConns: "9",
	}))
	if err == nil {
		t.Fatal("a pool with more idle than open connections was accepted")
	}
	for _, name := range []string{EnvDBMaxIdleConns, EnvDBMaxOpenConns} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error = %v, want it to mention %s", err, name)
		}
	}
}

func TestConnectionPoolRejectsUnusableValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		key   string
	}{
		{name: "zero open connections", key: EnvDBMaxOpenConns, value: "0"},
		{name: "negative open connections", key: EnvDBMaxOpenConns, value: "-2"},
		{name: "negative idle connections", key: EnvDBMaxIdleConns, value: "-1"},
		{name: "zero acquire timeout", key: EnvDBAcquireTimeout, value: "0s"},
		{name: "unparseable lifetime", key: EnvDBConnMaxLifetime, value: "half an hour"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{test.key: test.value}))
			if err == nil {
				t.Fatalf("%s=%q was accepted", test.key, test.value)
			}
			if !strings.Contains(err.Error(), test.key) {
				t.Errorf("error = %v, want it to name %s", err, test.key)
			}
		})
	}
}

// TestSecretsListsWhatMustNeverBeLogged covers the handoff to the logging
// layer: only a real credential is returned, and a URL without one returns
// nothing so redaction cannot erase unrelated text.
func TestSecretsListsWhatMustNeverBeLogged(t *testing.T) {
	for _, test := range []struct {
		name string
		dsn  string
		want []string
	}{
		{name: "password in the url", dsn: "postgres://user:sup3rsecret@db:5432/gitnest", want: []string{"sup3rsecret"}},
		{name: "percent-encoded password", dsn: "postgres://user:p%40ss@db:5432/gitnest", want: []string{"p@ss"}},
		{name: "no password", dsn: "postgres://user@db:5432/gitnest", want: nil},
		{name: "empty password", dsn: "postgres://user:@db:5432/gitnest", want: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := mustLoad(t, map[string]string{EnvDatabaseURL: test.dsn})
			got := cfg.Secrets()
			if len(got) != len(test.want) {
				t.Fatalf("Secrets() = %v, want %v", got, test.want)
			}
			for i := range test.want {
				if got[i] != test.want[i] {
					t.Errorf("Secrets()[%d] = %q, want %q", i, got[i], test.want[i])
				}
			}
		})
	}
}

// TestLoadReadsTheProcessEnvironment covers the thin wrapper: Load must see a
// real variable and LoadFrom must not.
func TestLoadReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv(EnvPort, "9099")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9099 {
		t.Fatalf("Load did not read the environment: Port = %d", cfg.Port)
	}
	if _, err := LoadFrom(empty()); err != nil {
		t.Fatalf("LoadFrom with an empty lookup: %v", err)
	}
}
