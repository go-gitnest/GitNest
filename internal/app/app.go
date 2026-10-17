// Package app wires one run mode of the GitNest process: it constructs the
// dependencies from configuration, owns their lifetime, and starts the server.
// It holds no domain rules.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/comments"
	"github.com/go-gitnest/GitNest/internal/config"
	"github.com/go-gitnest/GitNest/internal/gittransport"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/labels"
	"github.com/go-gitnest/GitNest/internal/milestones"
	platformgit "github.com/go-gitnest/GitNest/internal/platform/git"
	"github.com/go-gitnest/GitNest/internal/platform/postgres"
	"github.com/go-gitnest/GitNest/internal/platform/storage"
	"github.com/go-gitnest/GitNest/internal/platform/systemd"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/go-gitnest/GitNest/internal/transport/http/api"
	githttp "github.com/go-gitnest/GitNest/internal/transport/http/git"
	"github.com/go-gitnest/GitNest/internal/transport/http/middleware"
	"github.com/go-gitnest/GitNest/internal/transport/http/web"
	webassets "github.com/go-gitnest/GitNest/web"
)

// gitConcurrency returns the admission bound for concurrent Git processes.
//
// The performance document gives GIT-CONCURRENCY as `max(4, 2 x vCPU)` capped at
// 64. It is an instance setting once the settings registry exists; until then the
// documented formula is computed here so the bound is never unbounded.
func gitConcurrency() int {
	limit := 2 * runtime.NumCPU()
	if limit < 4 {
		limit = 4
	}
	if limit > 64 {
		limit = 64
	}
	return limit
}

// App owns the dependencies of one run mode.
type App struct {
	config config.Config
}

// New constructs the application root from validated configuration.
func New(cfg config.Config) *App {
	return &App{config: cfg}
}

// Run opens the dependencies, serves HTTP, and blocks until ctx is cancelled or
// the server stops. It is the entry point behind `gitnest serve`.
func (a *App) Run(ctx context.Context) error {
	logger := slog.Default()

	db, err := postgres.Open(ctx, a.config.DatabaseURL, DatabaseSettings(a.config))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	mux := http.NewServeMux()
	api.Register(mux, db)
	// The account endpoints share one service instance, so everything that asks
	// an access question — this transport today, others later — gets the same
	// answer from the same code.
	accounts := identity.NewService(identity.NewPostgresStore(db.DB), identity.Argon2idHasher{}).
		WithThrottling(identity.DefaultLoginFailureLimit, identity.DefaultInvitationFailureLimit)
	api.RegisterAccounts(mux, accounts, a.config.PublicURL)

	// Repository storage and the Git transport.
	//
	// The scratch area is cleared before anything serves, so data left by a
	// process that died mid-operation cannot outlive the restart.
	repositories := repository.NewService(repository.NewPostgresStore(db.DB))
	layout, err := storage.NewLayout(a.config.RepoRoot, a.config.TmpRoot)
	if err != nil {
		return err
	}
	if err := layout.EnsureTmpRoot(); err != nil {
		return err
	}
	if err := layout.CleanupTemp(); err != nil {
		return err
	}
	// The LFS staging sweep is the other half of the same rule: T-C5 requires that
	// temporary data be removed "on success, failure, cancellation, and process
	// restart". Each upload removes its own staging file, so this is for the run
	// that was killed mid-upload and left one behind. A failure is reported rather
	// than fatal: stale staging data is a disk-space concern, not a correctness
	// one, and refusing to start over it would turn a cleanup problem into an
	// outage.
	if removed, err := layout.CleanupLFSStaging(); err != nil {
		logger.Warn("the LFS staging sweep failed", "error", err)
	} else if removed > 0 {
		logger.Info("removed stale LFS staging files left by an earlier run", "count", removed)
	}
	runner, err := platformgit.NewRunner("git", layout, gitConcurrency())
	if err != nil {
		// Git is a deployment requirement, so a missing executable is a startup
		// failure with a clear message rather than a transport that fails later.
		return fmt.Errorf("prepare the Git command runner: %w", err)
	}

	// The content reader serves the browse surfaces. It shares the runner and the
	// layout with the Git transport, so a read through the API and a read through
	// the protocol cannot disagree about what the repository contains.
	content, err := gittransport.NewReader(runner, layout)
	if err != nil {
		return fmt.Errorf("prepare the repository content reader: %w", err)
	}
	api.RegisterRepositories(mux, accounts, accounts, repositories, content)
	api.RegisterSettings(mux, accounts, accounts, repositories)
	// The issue surface. It is built on the issue service, which answers its own access
	// questions through the same repository service the other transports call — so a page,
	// a payload, and a Git operation cannot disagree about who may see what.
	issueService := issues.NewService(repositories, issues.NewPostgresStore(db.DB))
	api.RegisterIssues(mux, accounts, accounts, repositories, issueService)

	// The comment surface, built on the comment service, which answers its own access questions
	// through the same repository service the other transports call. It depends on the issue service
	// because a comment is addressed under its issue's number, so the issue's identifier has to be
	// resolved — through the shared service, which checks the caller's access before it is known.
	commentService := comments.NewService(repositories, comments.NewPostgresStore(db.DB))
	api.RegisterComments(mux, accounts, accounts, repositories, commentService, issueService)

	// The label surface, built on the label service, which answers its own access questions through the
	// same repository service the other transports call. It depends on the issue service because a
	// label is applied under an issue's number.
	labelService := labels.NewService(repositories, labels.NewPostgresStore(db.DB))
	api.RegisterLabels(mux, accounts, accounts, repositories, labelService, issueService)

	// The milestone surface, built on the milestone service, which answers its own access questions through
	// the same repository service the other transports call. It needs repository **write** where the label
	// surface needs admin, because the catalogue puts milestones with the content events rather than the
	// configuration ones.
	milestoneService := milestones.NewService(repositories, milestones.NewPostgresStore(db.DB))
	api.RegisterMilestones(mux, accounts, accounts, repositories, milestoneService, issueService)

	// The server-rendered read surfaces. They share the same repository service
	// and content reader as the API, so a page and a payload cannot disagree
	// about what a caller may see.
	renderer, err := web.NewHandler(repositories, content, accounts, accounts, "")
	if err != nil {
		return fmt.Errorf("prepare the repository renderer: %w", err)
	}
	// The issue read surfaces share the issue service the API uses, so a page and a payload
	// answer the same access question, and the history is read from the same ledger the
	// domain writes to.
	// `WithCSRF` is what enables the issue mutation forms: a form without a token could never be
	// submitted, so the routes are registered only when an issuer is present. The issuer is the
	// identity service, whose token is the one the API and the interactive shells already verify.
	renderer = renderer.
		WithIssues(issueService).
		WithComments(commentService).
		WithLabels(labelService).
		WithMilestones(milestoneService).
		WithAccounts(accounts).
		WithAudit(audit.NewReader(db.DB)).
		WithCSRF(accounts)
	renderer.Register(mux)

	// The interactive surfaces. One bundle serves each area; the shell is
	// server-rendered, so a deep link works on first load and a reader without
	// JavaScript still sees a page that explains what it needs.
	builtAssets, err := webassets.FS()
	if err != nil {
		return fmt.Errorf("open the embedded frontend: %w", err)
	}
	frontend, err := web.NewAssets(builtAssets)
	if err != nil {
		return fmt.Errorf("read the frontend build: %w", err)
	}
	if !frontend.Built() {
		// Not fatal: the read surfaces work, and the interactive ones say what to
		// run. `go build` and the whole test suite have to work on a checkout
		// where the frontend has not been built.
		logger.Warn("the interactive frontend is not built; the interactive surfaces will report it",
			"fix", "npm install && npm run build in the web directory, then rebuild the server")
	}
	application, err := web.NewAppHandler(frontend, accounts, accounts)
	if err != nil {
		return fmt.Errorf("prepare the interactive surfaces: %w", err)
	}
	application.Register(mux)

	// The Git transport now authenticates a personal access token. This is the seam
	// W1.7 built and left anonymous on purpose ("a credential this server cannot verify
	// must not appear to authenticate anyone"), and it is what makes phase 1's exit
	// criterion — an invited contributor pushes to a private repository — reachable.
	//
	// A credential that cannot be verified still resolves to anonymous, so the transport
	// gains no existence oracle: the repository service refuses an anonymous caller with
	// the same 404 a nonexistent repository gets, exactly as before.
	gitAuthenticator, err := githttp.NewTokenAuthenticator(accounts,
		func(message string, attributes ...any) { logger.Warn(message, attributes...) })
	if err != nil {
		return fmt.Errorf("prepare the Git credential resolver: %w", err)
	}
	gitTransport, err := githttp.NewHandler(runner, layout, accounts, repositories,
		gitAuthenticator,
		func(message string, attributes ...any) { logger.Warn(message, attributes...) })
	if err != nil {
		return err
	}
	// The LFS batch returns absolute action URLs, and the configured public URL is
	// authoritative for them: it is correct behind a proxy, where the request's Host
	// is not the URL the client used.
	gitTransport.WithPublicURL(a.config.PublicURL)
	gitTransport.Register(mux)

	// The built assets are dispatched ahead of the mux: their prefix is a subtree
	// pattern that `net/http` cannot rank against the read surfaces' repository
	// routes, so registering it on the mux is a startup panic. See
	// web.AppHandler.AssetHandler.
	//
	// The dispatch happens *inside* the chain rather than outside it, so an asset
	// response carries the same security headers and the same access log line as
	// every other response. Wrapping from the outside would serve the assets
	// before the headers were set, which is the instance-wide guarantee the threat
	// model asks for and would not have held.
	dispatched := application.AssetHandler(renderer.StaticHandler(mux))

	// The correlation identifier is assigned before the access log runs, so a
	// request line and everything the handler logs share one identifier.
	//
	// The security headers wrap every response, because the threat model requires
	// them "instance-wide". They are the outermost middleware after the
	// correlation identifier so that a response produced by any surface carries
	// them, including one produced by a handler that never runs.
	handler := middleware.Chain(dispatched,
		middleware.RequestID,
		func(next http.Handler) http.Handler { return middleware.AccessLog(logger, next) },
		middleware.SecurityHeaders,
	)

	server := &http.Server{
		Addr:              a.config.HTTPAddress(),
		Handler:           handler,
		ReadHeaderTimeout: a.config.ReadHeaderTimeout,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	logger.Info("serving HTTP",
		slog.String("address", server.Addr),
		slog.String("public_url", a.config.PublicURL),
	)

	// systemd's readiness notification, when this process is a `Type=notify` unit.
	//
	// It is sent **after** the database has been confirmed reachable, not when the
	// listener opened, which is the whole reason the unit uses that type: a process
	// that is listening but cannot reach its database must not be reported ready, or
	// the supervisor would send it traffic. `db.Ready` is the same check `/api/ready`
	// answers, so the two cannot disagree.
	//
	// A failure to notify is logged rather than fatal. The process is serving either
	// way; the consequence is that systemd will time the unit out, which is a
	// deployment fault the operator sees, and failing the process as well would only
	// remove the diagnosis.
	readyCtx, cancelReady := context.WithTimeout(ctx, a.config.DBAcquireTimeout)
	if err := db.Ready(readyCtx); err != nil {
		logger.Warn("the database is not ready at startup; systemd will not be told the service is ready",
			slog.String("error", err.Error()))
	} else if err := systemd.Ready(); err != nil {
		logger.Warn("could not report readiness to systemd", slog.String("error", err.Error()))
	} else {
		logger.Info("reported readiness to systemd")
	}
	cancelReady()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP on %s: %w", server.Addr, err)
	case <-ctx.Done():
		logger.Info("stopping HTTP server",
			slog.String("address", server.Addr),
			slog.String("deadline", a.config.ShutdownTimeout.String()),
		)
		// A drain can outlast a person's patience, and `systemctl status` is where they
		// look while it runs. The message says what is happening rather than leaving
		// them with a service that appears stuck.
		if err := systemd.Stopping("draining in-flight requests and Git operations"); err != nil {
			logger.Debug("could not report stopping to systemd", slog.String("error", err.Error()))
		}
		shutdownErr := shutdownServer(server, a.config.ShutdownTimeout)

		// The listener is closed by Shutdown, or by Close when the deadline
		// expired; either way Serve has returned by now.
		if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) && err != nil {
			return fmt.Errorf("serve HTTP on %s: %w", server.Addr, err)
		}
		if shutdownErr != nil {
			// A failure to drain inside the deadline is a failed shutdown, so
			// the exit status tells an operator the difference.
			return shutdownErr
		}
		logger.Info("stopped HTTP server", slog.String("address", server.Addr))
		return nil
	}
}

// shutdownServer stops accepting new connections and waits for in-flight
// requests to finish, up to timeout. A request that outlives the deadline is
// cut off, and the deadline is reported as an error so the caller can fail the
// process rather than claim a clean stop.
//
// The database pool is not closed here: the caller's deferred close runs after
// this returns, so a request being drained still has a usable pool.
func shutdownServer(server *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		// Close the listener and every remaining connection so the process can
		// exit; Shutdown has already given up on them.
		_ = server.Close()
		return fmt.Errorf("drain in-flight requests within %s: %w", timeout, err)
	}
	return nil
}

// Migrate applies the given migration set to the configured database and
// reports what it did. It is the entry point behind `gitnest migrate`, and it
// takes the migration set as an argument so a test can supply its own.
func Migrate(ctx context.Context, cfg config.Config, files fs.FS) (postgres.Report, error) {
	db, err := postgres.Open(ctx, cfg.DatabaseURL, DatabaseSettings(cfg))
	if err != nil {
		return postgres.Report{}, err
	}
	defer func() { _ = db.Close() }()

	return db.Migrate(ctx, files)
}

// CreateInitialAdmin opens the configured database and creates the instance's
// first administrator. It is the entry point behind `gitnest admin create`.
func CreateInitialAdmin(ctx context.Context, cfg config.Config, input identity.CreateInitialAdminInput) (identity.User, error) {
	db, err := postgres.Open(ctx, cfg.DatabaseURL, DatabaseSettings(cfg))
	if err != nil {
		return identity.User{}, err
	}
	defer func() { _ = db.Close() }()

	service := identity.NewService(identity.NewPostgresStore(db.DB), identity.Argon2idHasher{})
	return service.CreateInitialAdmin(ctx, input)
}

// databaseSettings maps the startup configuration onto the pool budget.
// DatabaseSettings builds the bounded pool settings from configuration.
//
// It is exported so the CLI's migration preflight reaches the database through the
// same construction as the commands it is a preflight for: two constructions would
// be two places for a pool bound to drift, and the preflight must exercise the
// settings the real run will use.
func DatabaseSettings(cfg config.Config) postgres.Settings {
	return postgres.Settings{
		MaxOpenConns:    cfg.DBMaxOpenConns,
		MaxIdleConns:    cfg.DBMaxIdleConns,
		ConnMaxLifetime: cfg.DBConnMaxLifetime,
		ConnMaxIdleTime: cfg.DBConnMaxIdleTime,
		AcquireTimeout:  cfg.DBAcquireTimeout,
	}
}
