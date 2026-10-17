// Package middleware holds the HTTP wrappers every route passes through:
// correlation identifiers, request logging, and later authentication and rate
// limiting. A middleware here must not answer an authorization question; that
// belongs to a domain service.
package middleware

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-gitnest/GitNest/internal/platform/logging"
)

// CorrelationHeader carries the correlation identifier. A client may supply
// one, which makes a trace span across a proxy, but only when it looks like an
// identifier: accepting an arbitrary header value would let a caller inject
// newlines or forge another request's identifier in the log.
const CorrelationHeader = "X-Request-Id"

// correlationPattern bounds what a client-supplied identifier may contain.
var correlationPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// RequestID assigns a correlation identifier to every request, attaches it to
// the context, and echoes it in the response so a client can quote it in a bug
// report.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if !correlationPattern.MatchString(id) {
			id = logging.NewCorrelationID()
		}
		w.Header().Set(CorrelationHeader, id)
		next.ServeHTTP(w, r.WithContext(logging.WithCorrelationID(r.Context(), id)))
	})
}

// AccessLog writes one structured line per request.
//
// The query string is deliberately absent: it can carry a token or a password
// reset secret, and a route-level correlation identifier is enough to trace a
// request. The path alone is logged.
func AccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			// A handler that wrote nothing and set no status still succeeded.
			status = http.StatusOK
		}
		logger.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int("response_bytes", recorder.bytes),
			// Milliseconds as a number: slog renders a bare Duration as
			// nanoseconds, which an operator has to convert by hand.
			slog.Float64("duration_ms", float64(time.Since(started).Microseconds())/1000),
			slog.String("correlation_id", logging.CorrelationID(r.Context())),
		)
	})
}

// Chain applies middlewares so that the first listed is the outermost: the
// correlation identifier exists before anything else runs.
func Chain(handler http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// statusRecorder remembers the status and size a handler produced, which the
// access log needs and the standard ResponseWriter does not expose.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	written, err := r.ResponseWriter.Write(data)
	r.bytes += written
	return written, err
}

// Flush passes a flush through, so a streaming response such as a Git pack is
// not buffered by the middleware.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap exposes the original writer to http.ResponseController, so a handler
// that needs deadlines or hijacking keeps working.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
