package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/platform/logging"
)

// captureLogger returns a logger writing JSON into buf.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return logging.Configure(buf, slog.LevelDebug, logging.NewRedactor())
}

func TestRequestIDGeneratesAndEchoesAnIdentifier(t *testing.T) {
	var seen string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = logging.CorrelationID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	header := recorder.Header().Get(CorrelationHeader)
	if header == "" {
		t.Fatal("no correlation identifier was echoed")
	}
	if header != seen {
		t.Errorf("handler saw %q, response carried %q", seen, header)
	}
	if len(header) != 32 {
		t.Errorf("generated identifier %q is not a 16-byte hex string", header)
	}
}

func TestRequestIDAcceptsAWellFormedClientIdentifier(t *testing.T) {
	const clientID = "trace-0123456789abcdef"
	var seen string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = logging.CorrelationID(r.Context())
	}))

	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.Header.Set(CorrelationHeader, clientID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if seen != clientID {
		t.Errorf("handler saw %q, want the client's identifier so a trace spans the proxy", seen)
	}
	if got := recorder.Header().Get(CorrelationHeader); got != clientID {
		t.Errorf("response carried %q, want %q", got, clientID)
	}
}

// TestRequestIDReplacesAnUnsafeClientIdentifier stops a caller from injecting a
// newline into the log or forging another request's identifier.
func TestRequestIDReplacesAnUnsafeClientIdentifier(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "newline", value: "abc\ndef"},
		{name: "carriage return", value: "abc\rdef"},
		{name: "space", value: "abc def"},
		{name: "too long", value: strings.Repeat("a", 129)},
		{name: "unsafe characters", value: "abc\"def"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var seen string
			handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = logging.CorrelationID(r.Context())
			}))

			request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			request.Header.Set(CorrelationHeader, test.value)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if seen == test.value {
				t.Errorf("the unsafe identifier %q was accepted", test.value)
			}
			if len(seen) != 32 {
				t.Errorf("identifier %q was not replaced with a generated one", seen)
			}
		})
	}
}

func TestAccessLogRecordsOneLinePerRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("body"))
		}),
		RequestID,
		func(next http.Handler) http.Handler { return AccessLog(logger, next) },
	)

	request := httptest.NewRequest(http.MethodGet, "/api/ready?token=sup3rsecret", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	line := strings.TrimSpace(buf.String())
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("more than one line was written: %q", line)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("access log line is not JSON: %v (%q)", err, line)
	}

	if decoded["msg"] != "http_request" {
		t.Errorf("msg = %v, want http_request", decoded["msg"])
	}
	if decoded["method"] != http.MethodGet {
		t.Errorf("method = %v", decoded["method"])
	}
	if decoded["path"] != "/api/ready" {
		t.Errorf("path = %v, want the path without the query", decoded["path"])
	}
	if status, ok := decoded["status"].(float64); !ok || int(status) != http.StatusTeapot {
		t.Errorf("status = %v, want 418", decoded["status"])
	}
	if decoded["correlation_id"] == "" || decoded["correlation_id"] == nil {
		t.Error("no correlation identifier was recorded")
	}
	if _, ok := decoded["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms is missing or not a number: %v", decoded["duration_ms"])
	}
	// The query string can carry a token, so it must never reach the log.
	if strings.Contains(line, "sup3rsecret") {
		t.Errorf("the query string reached the log: %s", line)
	}
}

func TestAccessLogRecordsSuccessWhenTheHandlerWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)
	handler := AccessLog(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &decoded); err != nil {
		t.Fatalf("access log line is not JSON: %v", err)
	}
	if status, _ := decoded["status"].(float64); int(status) != http.StatusOK {
		t.Errorf("status = %v, want 200 for a handler that wrote nothing", decoded["status"])
	}
}

// TestAccessLogRedactsWhatTheHandlerLogs shows the two middlewares working
// together: the identifier is attached before the access line is written.
func TestAccessLogRedactsRegisteredSecrets(t *testing.T) {
	const secret = "sup3rsecret"
	var buf bytes.Buffer
	logger := logging.Configure(&buf, slog.LevelInfo, logging.NewRedactor(secret))

	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logging.Logger(r.Context()).Error("failed to connect", "dsn", "postgres://gitnest:"+secret+"@db/gitnest")
		}),
		RequestID,
		func(next http.Handler) http.Handler { return AccessLog(logger, next) },
	)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/ready", nil))

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("the secret reached the log: %s", buf.String())
	}
}

func TestChainAppliesTheFirstMiddlewareOutermost(t *testing.T) {
	var order []string
	mark := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	handler := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mark("first"), mark("second"))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestStatusRecorderKeepsTheUnderlyingWriterReachable protects streaming
// responses: a middleware must not take away flush or hijack access.
func TestStatusRecorderKeepsTheUnderlyingWriterReachable(t *testing.T) {
	original := httptest.NewRecorder()
	recorder := &statusRecorder{ResponseWriter: original}

	if recorder.Unwrap() != http.ResponseWriter(original) {
		t.Error("Unwrap did not return the original writer")
	}
	recorder.WriteHeader(http.StatusAccepted)
	if recorder.status != http.StatusAccepted {
		t.Errorf("status = %d, want 202", recorder.status)
	}
	// A second WriteHeader must not change the recorded status.
	recorder.WriteHeader(http.StatusInternalServerError)
	if recorder.status != http.StatusAccepted {
		t.Errorf("status = %d, want the first status to win", recorder.status)
	}
	_, _ = recorder.Write([]byte("payload"))
	if recorder.bytes != len("payload") {
		t.Errorf("bytes = %d, want %d", recorder.bytes, len("payload"))
	}
}
