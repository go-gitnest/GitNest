package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/platform/postgres"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
)

// maintenanceDSN returns the integration DSN and skips the test when none is
// configured, or fails it when the run requires PostgreSQL.
func maintenanceDSN(t *testing.T) string {
	t.Helper()
	return postgrestest.Require(t)
}

func TestOpenConnectsAndReportsReady(t *testing.T) {
	dsn := maintenanceDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := postgres.Open(ctx, dsn, postgres.DefaultSettings())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got := client.Settings(); got != postgres.DefaultSettings() {
		t.Errorf("Settings() = %+v, want the documented defaults", got)
	}
}

// TestOpenAppliesThePoolBounds proves the bounds reach a live pool, which the
// fake-based unit test cannot show.
func TestOpenAppliesThePoolBounds(t *testing.T) {
	dsn := maintenanceDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	settings := postgres.Settings{
		MaxOpenConns:    3,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
		AcquireTimeout:  2 * time.Second,
	}
	client, err := postgres.Open(ctx, dsn, settings)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close() }()

	if got := client.Settings(); got != settings {
		t.Fatalf("Settings() = %+v, want %+v", got, settings)
	}
	if err := client.Ready(ctx); err != nil {
		t.Fatalf("Ready with custom bounds: %v", err)
	}
}

// TestReadyFailsAfterClose covers the readiness path failing for a reason other
// than an unreachable host, and still reporting only the sentinel.
func TestReadyFailsAfterClose(t *testing.T) {
	dsn := maintenanceDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := postgres.Open(ctx, dsn, postgres.DefaultSettings())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	readyErr := client.Ready(ctx)
	if !errors.Is(readyErr, postgres.ErrNotReady) {
		t.Fatalf("Ready after Close = %v, want ErrNotReady", readyErr)
	}
	if readyErr.Error() != postgres.ErrNotReady.Error() {
		t.Errorf("Ready leaked detail: %q", readyErr.Error())
	}
}
