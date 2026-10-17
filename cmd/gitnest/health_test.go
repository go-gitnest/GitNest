package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The health command is the container's `HEALTHCHECK`, so its exit status is its whole
// interface: a wrong answer means a working container is killed or a broken one is
// kept. These tests drive it against a real HTTP server rather than a fake, because
// what is being tested is the probe, not the transport.

// probeServer answers one path with one status.
func probeServer(t *testing.T, path string, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not that path"))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// runHealth runs the command and returns its error.
//
// The streams are real buffers rather than a discard writer, because the package's
// other tests inspect what a command printed and this one may need to as well.
func runHealth(t *testing.T, args ...string) error {
	t.Helper()
	var out, errOut bytes.Buffer
	return healthCommand(context.Background(), args, stdio{out: &out, err: &errOut})
}

func TestHealthSucceedsAgainstAHealthyEndpoint(t *testing.T) {
	server := probeServer(t, "/api/health", http.StatusOK, `{"status":"ok"}`)

	if err := runHealth(t, "--url", server.URL); err != nil {
		t.Errorf("a healthy instance was reported unhealthy: %v", err)
	}
}

// TestHealthFailsOnANonOKStatus is the case that matters: a process that answers but
// is not well must not be reported healthy.
func TestHealthFailsOnANonOKStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusServiceUnavailable,
		http.StatusInternalServerError,
		http.StatusNotFound,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := probeServer(t, "/api/health", status, `{"status":"unavailable"}`)
			if err := runHealth(t, "--url", server.URL); err == nil {
				t.Errorf("a %d response was reported healthy", status)
			}
		})
	}
}

// TestHealthCarriesTheEndpointMessage keeps the diagnosis: "unhealthy" alone tells an
// operator nothing, while the endpoint's own line names the dependency that is down.
func TestHealthCarriesTheEndpointMessage(t *testing.T) {
	server := probeServer(t, "/api/ready", http.StatusServiceUnavailable, `{"status":"unavailable"}`)

	err := runHealth(t, "--url", server.URL, "--ready")
	if err == nil {
		t.Fatal("an unavailable readiness endpoint was reported healthy")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("the error does not carry the endpoint's message: %v", err)
	}
	// And it names the endpoint, so an operator knows which probe answered.
	if !strings.Contains(err.Error(), "/api/ready") {
		t.Errorf("the error does not name the endpoint: %v", err)
	}
}

// TestHealthProbesReadinessWhenAsked covers the flag a scheduler should use: readiness
// reports database availability, which health does not.
func TestHealthProbesReadinessWhenAsked(t *testing.T) {
	// A server that answers health but not readiness is exactly the state the
	// distinction exists for: alive, and unable to serve.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			w.WriteHeader(http.StatusOK)
		case "/api/ready":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	if err := runHealth(t, "--url", server.URL); err != nil {
		t.Errorf("health should pass while the process is alive: %v", err)
	}
	if err := runHealth(t, "--url", server.URL, "--ready"); err == nil {
		t.Error("readiness should fail while the database is unreachable")
	}
}

// TestHealthFailsWhenNothingListens covers a process that is not up yet, which is the
// state during a container's start period.
func TestHealthFailsWhenNothingListens(t *testing.T) {
	// Port 1 has nothing listening.
	if err := runHealth(t, "--url", "http://127.0.0.1:1"); err == nil {
		t.Error("a probe against a closed port succeeded")
	}
}

// TestHealthRejectsBadArguments keeps a misconfigured probe from being read as a
// health problem.
func TestHealthRejectsBadArguments(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "a negative timeout", args: []string{"--url", "http://127.0.0.1:1", "--timeout", "-1s"}},
		{name: "a positional argument", args: []string{"extra"}},
		{name: "an unknown flag", args: []string{"--nonsense"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := runHealth(t, test.args...); err == nil {
				t.Error("bad arguments were accepted")
			}
		})
	}
}

// TestHealthUsesTheConfiguredListener covers the default: no `--url` means the probe
// follows the instance's own configuration, so a deployment that bound elsewhere does
// not need the probe restated in two places.
func TestHealthUsesTheConfiguredListener(t *testing.T) {
	// The default path is asserted through a listener bound to a wildcard address,
	// which is what a container image sets: a probe that used the wildcard literally
	// would fail to connect and report a healthy instance unhealthy.
	t.Setenv("GITNEST_HOST", "0.0.0.0")
	t.Setenv("GITNEST_PORT", "1")

	err := runHealth(t)
	if err == nil {
		t.Fatal("a probe against a closed port succeeded")
	}
	// The address it tried is the loopback one, not the wildcard.
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the probe did not use the loopback address for a wildcard bind: %v", err)
	}
	// It probed health rather than readiness.
	if !strings.Contains(err.Error(), "/api/health") {
		t.Errorf("the default probe is not /api/health: %v", err)
	}
}

// TestHealthRejectsAnUnparseableURL keeps a typo in the environment from looking like
// an unhealthy instance.
func TestHealthRejectsAnUnparseableURL(t *testing.T) {
	err := runHealth(t, "--url", "://nonsense")
	if err == nil {
		t.Fatal("an unparseable URL was accepted")
	}
	// It is reported as a request-building failure, not as a probe failure, so an
	// operator can tell a typo from an outage.
	if !strings.Contains(err.Error(), "build the probe request") {
		t.Errorf("the error does not distinguish a bad URL from a failed probe: %v", err)
	}
}

// TestHealthWithAContextDeadlineIsNotAHang keeps the probe bounded even when the
// endpoint accepts the connection and never answers.
func TestHealthWithAContextDeadlineIsNotAHang(t *testing.T) {
	// A server that never writes a response.
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	t.Cleanup(func() {
		close(blocked)
		server.Close()
	})

	err := runHealth(t, "--url", server.URL, "--timeout", "50ms")
	if err == nil {
		t.Fatal("a probe against a non-answering endpoint succeeded")
	}
	// A timeout is reported, rather than the command hanging until the container
	// runtime's own deadline.
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "probe") {
		t.Errorf("the error does not describe a timed-out probe: %v", err)
	}
}
