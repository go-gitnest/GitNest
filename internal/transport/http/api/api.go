// Package api serves the operational HTTP endpoints.
//
// Liveness and readiness are unauthenticated, so their responses are a fixed,
// small shape: a status and the build identity. They never echo a connection
// string, a dependency error, a configuration value, or a filesystem path.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-gitnest/GitNest/internal/buildinfo"
)

// ReadinessChecker reports whether a dependency the process needs is usable.
// The PostgreSQL client implements it; a test supplies its own.
type ReadinessChecker interface {
	Ready(ctx context.Context) error
}

// readinessTimeout bounds a readiness check even when the checker does not
// bound itself, so a hung dependency cannot hang the probe. The checker's own
// timeout is normally shorter.
const readinessTimeout = 3 * time.Second

// status is the body of both operational endpoints. It deliberately carries no
// dependency detail: a probe is reachable without authentication.
type status struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Register adds the operational endpoints to mux.
//
//	GET /api/health  liveness: the process is running
//	GET /api/ready   readiness: the database answers, else 503
func Register(mux *http.ServeMux, db ReadinessChecker) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusOK, "ok")
	})

	mux.HandleFunc("GET /api/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := db.Ready(ctx); err != nil {
			// The error is dropped on purpose: it can contain a host, a
			// database name, or driver detail, and this endpoint is public.
			// Operators get the detail from the startup log instead.
			writeStatus(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		writeStatus(w, http.StatusOK, "ready")
	})
}

func writeStatus(w http.ResponseWriter, code int, value string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A probe answer is a live measurement and must never be cached.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)

	info := buildinfo.Current()
	_ = json.NewEncoder(w).Encode(status{
		Status:  value,
		Version: info.Version,
		Commit:  info.Commit,
	})
}
