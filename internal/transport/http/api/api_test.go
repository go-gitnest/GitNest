package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/buildinfo"
)

// stubChecker reports a fixed readiness result and records how it was called.
type stubChecker struct {
	err   error
	calls int
}

func (s *stubChecker) Ready(context.Context) error {
	s.calls++
	return s.err
}

func TestLivenessReportsTheProcessIsRunning(t *testing.T) {
	checker := &stubChecker{}
	recorder := serve(t, checker, http.MethodGet, "/api/health")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := decodeStatus(t, recorder)
	if body.Status != "ok" {
		t.Errorf("status field = %q, want ok", body.Status)
	}
	// Liveness must not touch the database: a dependency check here would make
	// the process look dead when only the database is down.
	if checker.calls != 0 {
		t.Errorf("liveness called the readiness checker %d times, want 0", checker.calls)
	}
}

func TestReadinessReflectsTheDependency(t *testing.T) {
	for _, test := range []struct {
		name       string
		checkerErr error
		wantCode   int
		wantStatus string
	}{
		{name: "database reachable", checkerErr: nil, wantCode: http.StatusOK, wantStatus: "ready"},
		{name: "database unavailable", checkerErr: errors.New("dial tcp 10.0.0.5:5432: connection refused"), wantCode: http.StatusServiceUnavailable, wantStatus: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checker := &stubChecker{err: test.checkerErr}
			recorder := serve(t, checker, http.MethodGet, "/api/ready")

			if recorder.Code != test.wantCode {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantCode)
			}
			if body := decodeStatus(t, recorder); body.Status != test.wantStatus {
				t.Errorf("status field = %q, want %q", body.Status, test.wantStatus)
			}
			if checker.calls != 1 {
				t.Errorf("readiness checker called %d times, want 1", checker.calls)
			}
		})
	}
}

// TestResponsesDiscloseNothing is the acceptance criterion: a probe answer must
// not carry a connection string, a dependency error, or anything else an
// unauthenticated caller can use.
func TestResponsesDiscloseNothing(t *testing.T) {
	const leak = "postgres://gitnest:sup3rsecret@10.0.0.5:5432/gitnest"
	checker := &stubChecker{err: errors.New("connect to " + leak + ": password authentication failed")}

	for _, path := range []string{"/api/health", "/api/ready"} {
		t.Run(path, func(t *testing.T) {
			recorder := serve(t, checker, http.MethodGet, path)
			body := recorder.Body.String()

			for _, secret := range []string{"sup3rsecret", "postgres://", "10.0.0.5", "password authentication"} {
				if strings.Contains(body, secret) {
					t.Errorf("response disclosed %q: %s", secret, body)
				}
			}
		})
	}
}

// TestResponseShapeIsFixed pins the exact body: adding a field here is a
// deliberate decision, not an accident.
func TestResponseShapeIsFixed(t *testing.T) {
	recorder := serve(t, &stubChecker{}, http.MethodGet, "/api/ready")

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := map[string]bool{"status": true, "version": true, "commit": true}
	for field := range decoded {
		if !want[field] {
			t.Errorf("unexpected field %q in the probe response", field)
		}
	}
	for field := range want {
		if _, ok := decoded[field]; !ok {
			t.Errorf("missing field %q in the probe response", field)
		}
	}
}

func TestResponsesCarryTheBuildIdentity(t *testing.T) {
	recorder := serve(t, &stubChecker{}, http.MethodGet, "/api/health")
	body := decodeStatus(t, recorder)

	info := buildinfo.Current()
	if body.Version != info.Version || body.Commit != info.Commit {
		t.Errorf("build identity = %s/%s, want %s/%s", body.Version, body.Commit, info.Version, info.Commit)
	}
}

func TestProbeResponsesAreNotCacheable(t *testing.T) {
	for _, path := range []string{"/api/health", "/api/ready"} {
		t.Run(path, func(t *testing.T) {
			recorder := serve(t, &stubChecker{}, http.MethodGet, path)
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, want JSON", got)
			}
		})
	}
}

func TestProbeEndpointsRejectOtherMethods(t *testing.T) {
	for _, path := range []string{"/api/health", "/api/ready"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			t.Run(method+" "+path, func(t *testing.T) {
				recorder := serve(t, &stubChecker{}, method, path)
				if recorder.Code != http.StatusMethodNotAllowed {
					t.Errorf("status = %d, want 405", recorder.Code)
				}
			})
		}
	}
}

func TestUnknownPathsAreNotFound(t *testing.T) {
	recorder := serve(t, &stubChecker{}, http.MethodGet, "/api/healthz")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}
}

// serve exercises one request against a mux with the endpoints registered.
func serve(t *testing.T, checker ReadinessChecker, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux, checker)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func decodeStatus(t *testing.T, recorder *httptest.ResponseRecorder) status {
	t.Helper()
	var decoded status
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return decoded
}
