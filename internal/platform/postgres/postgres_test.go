package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// recordedBounds records which pool bounds were applied, so a test can prove
// that none of them was left to the driver default.
type recordedBounds struct {
	calls    []string
	maxOpen  int
	maxIdle  int
	lifetime time.Duration
	idleTime time.Duration
}

func (r *recordedBounds) SetMaxOpenConns(n int) {
	r.calls = append(r.calls, "maxOpen")
	r.maxOpen = n
}

func (r *recordedBounds) SetMaxIdleConns(n int) {
	r.calls = append(r.calls, "maxIdle")
	r.maxIdle = n
}

func (r *recordedBounds) SetConnMaxLifetime(d time.Duration) {
	r.calls = append(r.calls, "lifetime")
	r.lifetime = d
}

func (r *recordedBounds) SetConnMaxIdleTime(d time.Duration) {
	r.calls = append(r.calls, "idleTime")
	r.idleTime = d
}

func TestDefaultSettingsMatchTheDocumentedBudget(t *testing.T) {
	settings := DefaultSettings()

	if settings.MaxOpenConns != 20 {
		t.Errorf("MaxOpenConns = %d, want 20 (DB-POOL-WEB)", settings.MaxOpenConns)
	}
	if settings.MaxIdleConns != 5 {
		t.Errorf("MaxIdleConns = %d, want 5 (DB-IDLE)", settings.MaxIdleConns)
	}
	if settings.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("ConnMaxLifetime = %s, want 30m (DB-LIFETIME)", settings.ConnMaxLifetime)
	}
	if settings.ConnMaxIdleTime != 5*time.Minute {
		t.Errorf("ConnMaxIdleTime = %s, want 5m (DB-IDLE-TIME)", settings.ConnMaxIdleTime)
	}
	if settings.AcquireTimeout != 2*time.Second {
		t.Errorf("AcquireTimeout = %s, want 2s (DB-ACQUIRE)", settings.AcquireTimeout)
	}
	if err := settings.Validate(); err != nil {
		t.Errorf("the documented defaults are not valid: %v", err)
	}
}

// TestApplySettingsSetsEveryBound is the acceptance criterion that the pool is
// configured with explicit maximum open and idle connections and a connection
// lifetime.
func TestApplySettingsSetsEveryBound(t *testing.T) {
	settings := Settings{
		MaxOpenConns:    7,
		MaxIdleConns:    3,
		ConnMaxLifetime: 11 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute,
		AcquireTimeout:  time.Second,
	}
	var bounds recordedBounds
	applySettings(&bounds, settings)

	for _, want := range []string{"maxOpen", "maxIdle", "lifetime", "idleTime"} {
		found := false
		for _, call := range bounds.calls {
			if call == want {
				found = true
			}
		}
		if !found {
			t.Errorf("bound %q was never set; calls = %v", want, bounds.calls)
		}
	}
	if bounds.maxOpen != 7 || bounds.maxIdle != 3 {
		t.Errorf("open/idle = %d/%d, want 7/3", bounds.maxOpen, bounds.maxIdle)
	}
	if bounds.lifetime != 11*time.Minute || bounds.idleTime != 2*time.Minute {
		t.Errorf("lifetime/idleTime = %s/%s, want 11m/2m", bounds.lifetime, bounds.idleTime)
	}
}

// TestZeroIdleConnectionsIsAllowed pins that zero is a real value, not "unset":
// a deployment may choose not to keep idle connections.
func TestZeroIdleConnectionsIsAllowed(t *testing.T) {
	settings := DefaultSettings()
	settings.MaxIdleConns = 0
	if err := settings.Validate(); err != nil {
		t.Fatalf("idle connections of zero were rejected: %v", err)
	}
}

// TestZeroValueSettingsAreRejected pins that a caller must start from
// DefaultSettings rather than relying on silent filling.
func TestZeroValueSettingsAreRejected(t *testing.T) {
	err := Settings{}.Validate()
	if err == nil {
		t.Fatal("the zero Settings value was accepted")
	}
	if !strings.Contains(err.Error(), "maximum open connections") {
		t.Errorf("error = %v, want it to name the first missing bound", err)
	}
}

func TestSettingsValidation(t *testing.T) {
	valid := DefaultSettings()
	for _, test := range []struct {
		name     string
		mutate   func(Settings) Settings
		wantErr  bool
		contains string
	}{
		{name: "defaults", mutate: func(s Settings) Settings { return s }},
		{name: "no idle connections", mutate: func(s Settings) Settings { s.MaxIdleConns = 0; return s }},
		{name: "no open connections", mutate: func(s Settings) Settings { s.MaxOpenConns = 0; return s }, wantErr: true, contains: "maximum open"},
		{name: "negative idle", mutate: func(s Settings) Settings { s.MaxIdleConns = -1; return s }, wantErr: true, contains: "maximum idle"},
		{name: "idle exceeds open", mutate: func(s Settings) Settings { s.MaxIdleConns = s.MaxOpenConns + 1; return s }, wantErr: true, contains: "must not exceed"},
		{name: "no lifetime", mutate: func(s Settings) Settings { s.ConnMaxLifetime = 0; return s }, wantErr: true, contains: "lifetime"},
		{name: "no idle time", mutate: func(s Settings) Settings { s.ConnMaxIdleTime = 0; return s }, wantErr: true, contains: "idle time"},
		{name: "no acquire timeout", mutate: func(s Settings) Settings { s.AcquireTimeout = 0; return s }, wantErr: true, contains: "acquire"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.mutate(valid).Validate()
			if test.wantErr && err == nil {
				t.Fatal("invalid settings were accepted")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("valid settings were rejected: %v", err)
			}
			if test.wantErr && !strings.Contains(err.Error(), test.contains) {
				t.Errorf("error = %v, want it to mention %q", err, test.contains)
			}
		})
	}
}

func TestSafeTargetDescribesHostAndDatabaseOnly(t *testing.T) {
	for _, test := range []struct {
		name string
		dsn  string
		want string
	}{
		{name: "url with credentials", dsn: "postgres://user:sup3rsecret@db.example.com:5432/gitnest?sslmode=disable", want: "db.example.com:5432/gitnest"},
		{name: "url without database", dsn: "postgres://user:secret@db.example.com:5432", want: "db.example.com:5432"},
		{name: "no host", dsn: "postgres:///gitnest", want: "the configured database"},
		{name: "not a url", dsn: "host=db password=secret", want: "the configured database"},
		{name: "empty", dsn: "", want: "the configured database"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := safeTarget(test.dsn)
			if got != test.want {
				t.Errorf("safeTarget(%q) = %q, want %q", test.dsn, got, test.want)
			}
			for _, secret := range []string{"sup3rsecret", "secret"} {
				if strings.Contains(got, secret) {
					t.Errorf("safeTarget leaked %q: %q", secret, got)
				}
			}
		})
	}
}

func TestRedactRemovesEverySecretForm(t *testing.T) {
	for _, test := range []struct {
		name    string
		dsn     string
		message string
		secrets []string
	}{
		{
			name:    "url form",
			dsn:     "postgres://user:sup3rsecret@db/gitnest",
			message: `failed to connect: password authentication failed for "sup3rsecret"`,
			secrets: []string{"sup3rsecret"},
		},
		{
			name:    "keyword value form",
			dsn:     "host=db user=gitnest password=kvsecret dbname=gitnest",
			message: "connect failed with password=kvsecret for user gitnest",
			secrets: []string{"kvsecret"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			redacted := redact(test.message, test.dsn)
			for _, secret := range test.secrets {
				if strings.Contains(redacted, secret) {
					t.Errorf("redact left %q in %q", secret, redacted)
				}
			}
			if !strings.Contains(redacted, "REDACTED") {
				t.Errorf("redact(%q) = %q, want the secret replaced", test.message, redacted)
			}
		})
	}
}

func TestSecretsFindsPasswordsOnly(t *testing.T) {
	found := secrets("postgres://user:sup3rsecret@db:5432/gitnest")
	if len(found) != 1 || found[0] != "sup3rsecret" {
		t.Errorf("secrets() = %v, want [sup3rsecret]", found)
	}
	if got := secrets("postgres://db/gitnest"); len(got) != 0 {
		t.Errorf("secrets() without a password = %v, want empty", got)
	}
}

// TestOpenFailsFastWithoutLeakingThePassword covers the acceptance criterion for
// an unreachable database.
func TestOpenFailsFastWithoutLeakingThePassword(t *testing.T) {
	const password = "sup3rsecret"
	dsn := "postgres://gitnest:" + password + "@127.0.0.1:1/gitnest?sslmode=disable&connect_timeout=1"

	started := time.Now()
	settings := DefaultSettings()
	settings.AcquireTimeout = time.Second
	client, err := Open(context.Background(), dsn, settings)
	elapsed := time.Since(started)

	if err == nil {
		_ = client.Close()
		t.Fatal("Open succeeded against a closed port")
	}
	if client != nil {
		t.Error("Open returned a client alongside an error")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1/gitnest") {
		t.Errorf("error = %v, want it to name the target so an operator can fix it", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Open took %s; the failure must be bounded by the acquire timeout", elapsed)
	}
}

func TestOpenRejectsInvalidSettingsBeforeConnecting(t *testing.T) {
	client, err := Open(context.Background(), "postgres://user@127.0.0.1:1/gitnest", Settings{MaxIdleConns: 99, MaxOpenConns: 1})
	if err == nil {
		_ = client.Close()
		t.Fatal("Open accepted settings whose idle bound exceeds the open bound")
	}
	if !strings.Contains(err.Error(), "invalid database settings") {
		t.Errorf("error = %v, want it to name the settings problem", err)
	}
}

// TestReadyReportsFailureGenerically covers the readiness criterion: the
// endpoint must not be able to leak connection details.
func TestReadyReportsFailureGenerically(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://gitnest:sup3rsecret@127.0.0.1:1/gitnest?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer func() { _ = db.Close() }()

	client := &Client{DB: db, settings: Settings{AcquireTimeout: time.Second}}
	readyErr := client.Ready(context.Background())

	if !errors.Is(readyErr, ErrNotReady) {
		t.Fatalf("Ready() = %v, want ErrNotReady", readyErr)
	}
	if readyErr.Error() != ErrNotReady.Error() {
		t.Errorf("Ready() = %q, want the bare sentinel so no detail can leak", readyErr.Error())
	}
}

// TestClientReportsTheSettingsItWasOpenedWith keeps the effective bounds
// observable, which the operator console and tests both need.
func TestClientReportsTheSettingsItWasOpenedWith(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://gitnest@127.0.0.1:1/gitnest")
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer func() { _ = db.Close() }()

	settings := Settings{MaxOpenConns: 6, MaxIdleConns: 2, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, AcquireTimeout: time.Second}
	client := &Client{DB: db, settings: settings}
	if client.Settings() != settings {
		t.Errorf("Settings() = %+v, want %+v", client.Settings(), settings)
	}
}
