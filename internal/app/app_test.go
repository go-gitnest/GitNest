package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-gitnest/GitNest/internal/config"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
)

// freePort reserves and releases a local port so a test can bind it.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return port
}

// testConfig builds a complete startup configuration. Every field is set,
// because the pool settings are validated when the database is opened.
func testConfig(t *testing.T, dsn string, port int) config.Config {
	t.Helper()
	dataDir := t.TempDir()
	return config.Config{
		Host:              "127.0.0.1",
		Port:              port,
		PublicURL:         fmt.Sprintf("http://127.0.0.1:%d", port),
		DatabaseURL:       dsn,
		DataDir:           dataDir,
		RepoRoot:          filepath.Join(dataDir, "repositories"),
		TmpRoot:           filepath.Join(dataDir, "tmp"),
		LogLevel:          config.LogLevelInfo,
		SSHListen:         ":2222",
		ReadHeaderTimeout: 5 * time.Second,
		ShutdownTimeout:   10 * time.Second,
		DBMaxOpenConns:    5,
		DBMaxIdleConns:    2,
		DBConnMaxLifetime: 10 * time.Minute,
		DBConnMaxIdleTime: 3 * time.Minute,
		DBAcquireTimeout:  1500 * time.Millisecond,
	}
}

// maintenanceDSN returns the integration DSN and skips the test when none is
// configured.
func maintenanceDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GITNEST_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set GITNEST_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	return dsn
}

// TestDatabaseSettingsCarryEveryBound closes the loop between the configuration
// layer and the pool: every bound must cross the boundary unchanged, and the
// result must be a valid pool configuration.
func TestDatabaseSettingsCarryEveryBound(t *testing.T) {
	cfg := testConfig(t, "postgres://gitnest@127.0.0.1:1/gitnest", 8080)

	settings := DatabaseSettings(cfg)

	if settings.MaxOpenConns != cfg.DBMaxOpenConns {
		t.Errorf("MaxOpenConns = %d, want %d", settings.MaxOpenConns, cfg.DBMaxOpenConns)
	}
	if settings.MaxIdleConns != cfg.DBMaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want %d", settings.MaxIdleConns, cfg.DBMaxIdleConns)
	}
	if settings.ConnMaxLifetime != cfg.DBConnMaxLifetime {
		t.Errorf("ConnMaxLifetime = %s, want %s", settings.ConnMaxLifetime, cfg.DBConnMaxLifetime)
	}
	if settings.ConnMaxIdleTime != cfg.DBConnMaxIdleTime {
		t.Errorf("ConnMaxIdleTime = %s, want %s", settings.ConnMaxIdleTime, cfg.DBConnMaxIdleTime)
	}
	if settings.AcquireTimeout != cfg.DBAcquireTimeout {
		t.Errorf("AcquireTimeout = %s, want %s", settings.AcquireTimeout, cfg.DBAcquireTimeout)
	}
	if err := settings.Validate(); err != nil {
		t.Errorf("the mapped settings are not a valid pool configuration: %v", err)
	}
}

// TestRunRejectsAnImpossiblePoolConfiguration checks that an inconsistent
// configuration fails before anything listens, with the settings problem named.
func TestRunRejectsAnImpossiblePoolConfiguration(t *testing.T) {
	cfg := testConfig(t, "postgres://gitnest@127.0.0.1:1/gitnest", freePort(t))
	cfg.DBMaxOpenConns = 2
	cfg.DBMaxIdleConns = 3

	err := New(cfg).Run(context.Background())
	if err == nil {
		t.Fatal("Run accepted a configuration whose idle bound exceeds its open bound")
	}
	if !strings.Contains(err.Error(), "invalid database settings") {
		t.Errorf("error = %v, want it to report the settings problem", err)
	}
}

// TestRunFailsFastWhenTheDatabaseIsUnavailable covers startup ordering: the
// server must not begin listening when its database is unreachable, and the
// error must not carry the password.
func TestRunFailsFastWhenTheDatabaseIsUnavailable(t *testing.T) {
	const password = "sup3rsecret"
	port := freePort(t)
	cfg := testConfig(t, fmt.Sprintf("postgres://gitnest:%s@127.0.0.1:1/gitnest?sslmode=disable&connect_timeout=1", password), port)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	err := New(cfg).Run(ctx)
	if err == nil {
		t.Fatal("Run started with an unreachable database")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("error leaks the password: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("startup failure took %s; the connection attempt must be bounded", elapsed)
	}
	if conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); dialErr == nil {
		_ = conn.Close()
		t.Error("a listener accepted a connection after a failed start")
	}
}

// TestRunServesHealthAndReadiness is the end-to-end acceptance path: the server
// starts against a real database, both probes answer 200, and cancelling the
// context stops it and releases the listener.
func TestRunServesHealthAndReadiness(t *testing.T) {
	port := freePort(t)
	cfg := testConfig(t, maintenanceDSN(t), port)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	ctx, cancel := context.WithCancel(context.Background())
	// The completion signal is a closed channel rather than a single value, so
	// both the test body and the cleanup can observe it. A buffered channel
	// would be drained by whichever of them read it first, leaving the other to
	// block until its timeout.
	var runErr error
	stopped := make(chan struct{})
	go func() {
		runErr = New(cfg).Run(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(15 * time.Second):
			t.Error("the server did not stop during cleanup")
		}
	})

	waitForStatus(t, base+"/api/health", http.StatusOK, 15*time.Second)
	waitForStatus(t, base+"/api/ready", http.StatusOK, 15*time.Second)

	body := getBody(t, base+"/api/ready")
	for _, want := range []string{`"status":"ready"`, `"version"`, `"commit"`} {
		if !strings.Contains(body, want) {
			t.Errorf("readiness body %q does not contain %q", body, want)
		}
	}

	started := time.Now()
	cancel()
	select {
	case <-stopped:
		if runErr != nil {
			t.Fatalf("Run returned %v after cancellation, want nil", runErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if elapsed := time.Since(started); elapsed > cfg.ShutdownTimeout {
		t.Errorf("shutdown took %s, exceeding the configured %s deadline", elapsed, cfg.ShutdownTimeout)
	}
	if _, err := http.Get(base + "/api/health"); err == nil {
		t.Error("the listener still answers after Run returned")
	}
}

// TestRunReportsABusyPort checks that a port collision is a clear startup
// failure rather than a silent success.
func TestRunReportsABusyPort(t *testing.T) {
	dsn := maintenanceDSN(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	cfg := testConfig(t, dsn, listener.Addr().(*net.TCPAddr).Port)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runErr := New(cfg).Run(ctx)
	if runErr == nil {
		t.Fatal("Run reported success while the port was already in use")
	}
	if !strings.Contains(runErr.Error(), "serve HTTP") {
		t.Errorf("error = %v, want it to name the failing step", runErr)
	}
}

// TestCreateInitialAdminCommand covers the operator path end to end against a
// migrated database: a normalised account is created, its password verifies,
// and a second attempt is refused.
func TestCreateInitialAdminCommand(t *testing.T) {
	schema := postgrestest.NewSchema(t)
	cfg := testConfig(t, schemaDSN(t, schema), freePort(t))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const password = "correct horse battery staple"
	user, err := CreateInitialAdmin(ctx, cfg, identity.CreateInitialAdminInput{
		Username: "Root",
		Email:    "Root@Example.com",
		Password: password,
	})
	if err != nil {
		t.Fatalf("CreateInitialAdmin: %v", err)
	}
	if user.Username != "Root" || user.Email != "root@example.com" {
		t.Errorf("created %q <%q>, want the display name preserved and the address normalized", user.Username, user.Email)
	}
	if !user.IsAdmin {
		t.Error("the created account is not an administrator")
	}

	// The stored hash must verify the password the operator entered.
	var hash string
	if err := schema.Client.DB.QueryRowContext(ctx,
		"SELECT password_hash FROM users WHERE id = $1", user.ID).Scan(&hash); err != nil {
		t.Fatalf("read the stored hash: %v", err)
	}
	ok, err := identity.VerifyPassword(hash, password)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("the stored hash does not verify the password that was entered")
	}

	if _, err := CreateInitialAdmin(ctx, cfg, identity.CreateInitialAdminInput{
		Username: "second", Email: "second@example.com", Password: password,
	}); !errors.Is(err, identity.ErrAdminExists) {
		t.Fatalf("second bootstrap = %v, want ErrAdminExists", err)
	}
}

// schemaDSN returns the maintenance URL scoped to the schema the harness
// created, so the application layer connects to the same schema as the test.
func schemaDSN(t *testing.T, schema *postgrestest.Schema) string {
	t.Helper()
	parsed, err := url.Parse(strings.TrimSpace(os.Getenv(postgrestest.EnvDatabaseURL)))
	if err != nil {
		t.Fatalf("parse the maintenance URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema.Name)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// TestCreateInitialAdminRequiresMigrations covers the operator-facing failure
// when the bootstrap runs before `gitnest migrate`.
func TestCreateInitialAdminRequiresMigrations(t *testing.T) {
	schema := postgrestest.NewEmptySchema(t)
	cfg := testConfig(t, schemaDSN(t, schema), freePort(t))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := CreateInitialAdmin(ctx, cfg, identity.CreateInitialAdminInput{
		Username: "root", Email: "root@example.com", Password: "correct horse battery staple",
	})
	if !errors.Is(err, identity.ErrSchemaNotMigrated) {
		t.Fatalf("error = %v, want ErrSchemaNotMigrated", err)
	}
}

// TestMigrateFailsFastWhenTheDatabaseIsUnavailable covers the `gitnest migrate`
// startup path: an unreachable database must be reported and must not hang.
func TestMigrateFailsFastWhenTheDatabaseIsUnavailable(t *testing.T) {
	cfg := testConfig(t, "postgres://gitnest@127.0.0.1:1/gitnest?sslmode=disable&connect_timeout=1", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	report, err := Migrate(ctx, cfg, fstest.MapFS{})
	if err == nil {
		t.Fatal("Migrate reported success against an unreachable database")
	}
	if len(report.Applied) != 0 {
		t.Errorf("a failed run reported %v as applied", report.Applied)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("migrate failure took %s; the attempt must be bounded", elapsed)
	}
}

// startTestServer starts an http.Server on a free port and returns it with the
// channel ListenAndServe reports on.
func startTestServer(t *testing.T, handler http.Handler) (*http.Server, <-chan error) {
	t.Helper()
	server := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", server.Addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return server, serveErr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the test server never started listening")
	return nil, nil
}

// TestShutdownDrainsAnInFlightRequest is the acceptance criterion for a graceful
// stop: a request already being served finishes before the server stops.
func TestShutdownDrainsAnInFlightRequest(t *testing.T) {
	release := make(chan struct{})
	server, serveErr := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold the request open until the test allows it to finish
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("finished"))
	}))

	type response struct {
		body string
		err  error
	}
	inFlight := make(chan response, 1)
	go func() {
		resp, err := http.Get("http://" + server.Addr + "/slow")
		if err != nil {
			inFlight <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		inFlight <- response{body: string(body), err: err}
	}()
	time.Sleep(100 * time.Millisecond)

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdownServer(server, 5*time.Second) }()

	// While the request is in flight, shutdown must still be waiting.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned %v while a request was still being served", err)
	default:
	}

	close(release)

	got := <-inFlight
	if got.err != nil {
		t.Fatalf("the in-flight request failed: %v", got.err)
	}
	if got.body != "finished" {
		t.Errorf("body = %q, want the drained request to have completed", got.body)
	}
	if err := <-shutdownDone; err != nil {
		t.Errorf("shutdown after draining = %v, want nil", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("ListenAndServe returned %v, want ErrServerClosed", err)
	}
}

// TestShutdownCutsOffARequestThatOutlivesTheDeadline covers the other half: the
// deadline is a deadline, and missing it is reported as a failed stop rather
// than a silent success.
func TestShutdownCutsOffARequestThatOutlivesTheDeadline(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	server, _ := startTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-blocked // never completes within the deadline
	}))

	go func() {
		resp, err := http.Get("http://" + server.Addr + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)

	const deadline = 200 * time.Millisecond
	started := time.Now()
	err := shutdownServer(server, deadline)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("shutdown reported success while a request outlived the deadline")
	}
	if !strings.Contains(err.Error(), "drain in-flight requests within 200ms") {
		t.Errorf("error = %v, want it to state the deadline that was missed", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("shutdown took %s, want it bounded by the %s deadline", elapsed, deadline)
	}
}

func waitForStatus(t *testing.T, url string, want int, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastStatus int
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
		} else {
			lastStatus = resp.StatusCode
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("GET %s: last status %d, last error %v; want %d", url, lastStatus, lastErr, want)
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}
