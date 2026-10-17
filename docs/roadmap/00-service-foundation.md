# Service foundation roadmap

**Phase:** 0
**Status:** not started
**Roadmap file:** docs/roadmap/00-service-foundation.md

## Purpose

Make the process predictable to configure, start, inspect, and stop before any product feature exists. This phase produces no user-visible behaviour; it produces the skeleton that every later phase depends on, plus the test harness and CI pipeline that keep the rest of the plan honest.

## Scope

Repository scaffolding, the command-line entry point, environment configuration, PostgreSQL connectivity, the migration runner, health and readiness endpoints, structured logging, bounded shutdown, the local development database recipe, the integration test harness, and the CI pipeline for the Go service. No product domains, no HTTP product routes, no frontend.

## Workstreams

### W0.1 Module scaffolding and command entry point
- **Status:** implemented
- **Deliverable:** `go.mod` for the module, `cmd/gitnest/main.go` that parses a command and dispatches without holding business logic, and a package layout matching [../architecture.md](../architecture.md) section 4.1 with no placeholder packages for later phases.
- **Depends on:** none
- **Acceptance:** `go build ./...` succeeds on a clean checkout; an unknown command exits non-zero with a usage message; `main` contains no domain logic.

### W0.2 Configuration loading and validation
- **Status:** implemented
- **Deliverable:** `internal/config` loading every startup value from the environment with defaults, validating them as a unit, including `GITNEST_PUBLIC_URL` (absolute, no credentials, path, query, or fragment, and `https` unless the host is loopback).
- **Depends on:** W0.1
- **Acceptance:** each variable is tested for its accepted and rejected shapes; an invalid value fails startup naming the variable; reading the configuration is a pure function of the environment.

### W0.3 PostgreSQL connectivity
- **Status:** implemented
- **Deliverable:** `internal/platform/postgres` opening a pool with bounded connection settings, a connectivity check with a timeout, and a `Ready` check suitable for the readiness endpoint.
- **Depends on:** W0.2
- **Acceptance:** the pool is configured with explicit maximum open and idle connections and a connection lifetime; an unreachable database fails fast with a message that does not include the DSN password; `Ready` reports failure without leaking connection details.

### W0.4 Migration runner
- **Status:** implemented
- **Deliverable:** an embedded migration set in `migrations/` with a runner that applies files in order, records each in a `schema_migrations` ledger, and runs each migration and its ledger row in one transaction. The `gitnest migrate` command exposes it.
- **Depends on:** W0.3
- **Acceptance:** applying to an empty database creates the schema and records every file; a second run is a no-op with unchanged ledger timestamps; a deliberately failing migration leaves neither its schema work nor its ledger row behind; a migration explicitly marked as running outside a transaction is handled without wrapping it. Rules are in [../architecture/data-model.md](../architecture/data-model.md).

### W0.5 Health and readiness endpoints
- **Status:** implemented
- **Deliverable:** `/api/health` for liveness and `/api/ready` for dependency readiness, returning a small JSON body and `503` when a dependency is unavailable.
- **Depends on:** W0.3
- **Acceptance:** liveness returns `200` while the process runs; readiness returns `200` with a reachable database and `503` without one; neither response discloses a connection string, database error, or version detail beyond what an operator needs.

### W0.6 Structured logging
- **Status:** implemented
- **Deliverable:** structured JSON logging configured from the log level, with request and job correlation identifiers and a single redaction boundary for secret values.
- **Note:** the correlation mechanism is in place and the request path uses it. The job path uses the same context helpers and is wired when `internal/jobs` (durable queue) lands; no job producer exists yet.
- **Deliverable:** structured JSON logging configured from the log level, with request and job correlation identifiers and a single redaction boundary for secret values.
- **Depends on:** W0.2
- **Acceptance:** a log line is machine-parseable; the level setting is honoured; a registered secret never appears in output, including encoded forms.

### W0.7 Bounded shutdown
- **Status:** implemented
- **Deliverable:** signal handling for interrupt and termination that stops accepting new work, drains in-flight requests, closes the database pool, and exits within the configured deadline.
- **Note:** interruption and termination are handled; a second signal restores the default handler, so it terminates the process immediately rather than extending the deadline. The drain path is covered by tests with an in-flight request; with only the two probe routes, an end-to-end second-signal observation needs a route that can outlive the deadline, which the Git transport brings.
- **Deliverable:** signal handling for interrupt and termination that stops accepting new work, drains in-flight requests, closes the database pool, and exits within the configured deadline.
- **Depends on:** W0.5
- **Acceptance:** a delivered interrupt causes exit within the configured deadline; the listener is closed afterwards; a second signal does not extend the deadline; the exit status distinguishes clean shutdown from failure.

### W0.8 Local development database recipe
- **Status:** implemented
- **Deliverable:** `compose.yaml` running PostgreSQL with the credentials the default configuration expects, plus README instructions to start, inspect, and remove it without installing PostgreSQL globally.
- **Note:** the container was not started for verification because no Docker daemon is reachable in this environment. The file is validated with `docker compose config`, and a test in `internal/config` asserts that the recipe's user, password, database, and published port match the compiled default `GITNEST_DATABASE_URL` and that a healthcheck and a named volume are declared. Starting the container is the one step a contributor with a Docker daemon must confirm.
- **Deliverable:** `compose.yaml` running PostgreSQL with the credentials the default configuration expects, plus README instructions to start, inspect, and remove it without installing PostgreSQL globally.
- **Depends on:** none
- **Acceptance:** `docker compose config` validates the file; following the README alone yields a database the service can migrate against; the documented removal command deletes the data volume.

### W0.9 Integration test harness
- **Status:** implemented
- **Note:** the cap `GITNEST_TEST_DB_CONCURRENCY` bounds schemas per process; parallel test binaries each hold their own, because bounding across processes would need a server-side slot lock. A cross-process cap is worth adding if CI is ever sized by database connections rather than by test runtime.
- **Deliverable:** the convention and its helper package `internal/platform/postgrestest`: database-backed tests live beside the code, skip unless `GITNEST_TEST_DATABASE_URL` is set, obtain a uniquely named schema from the helper, and drop that schema rather than a database.
- **Depends on:** W0.4
- **Acceptance:** the default `go test ./...` passes with no database and skips integration tests; with the variable set, each test is isolated and leaves no schema behind; a failing test still cleans up.

### W0.10 CI pipeline
- **Status:** implemented
- **Deliverable:** a workflow that checks formatting, runs `go vet ./...`, `go build ./...`, and `go test -race ./...` against a PostgreSQL service container, on every push and pull request.
- **Note:** the workflow has not run on a hosted runner. Its gate logic was verified locally by injecting the defect each job exists to catch — an unformatted file, a vet finding, a build error, and a failing test — and confirming the job fails, plus the development-database job by starting a database with the recipe's own credentials. Confirm the first hosted run before treating the stage as closed.
- **Deliverable:** a workflow that checks formatting, runs `go vet ./...`, `go build ./...`, and `go test -race ./...` against a PostgreSQL service container, on every push and pull request.
- **Depends on:** W0.9
- **Acceptance:** the workflow fails on unformatted code, a vet finding, a build failure, and a failing test; it passes on a clean tree; the database-backed tests actually run in CI rather than skipping. The frontend build job is added by the phase 1 workstream that introduces the frontend; see [../architecture/packaging.md](../architecture/packaging.md).

## Data and interfaces

Schema introduced: `schema_migrations` only. Packages introduced: `cmd/gitnest`, `internal/app`, `internal/config`, `internal/platform/{postgres,logging}`, `internal/transport/http/api`, `migrations/`. Configuration introduced: the full startup set in [../architecture/configuration.md](../architecture/configuration.md).

## Security notes

- Configuration failures must not echo secret values; the database URL's credentials never appear in an error or log line.
- Health and readiness endpoints are unauthenticated and therefore disclose nothing beyond liveness and dependency availability.
- The migration runner executes repository-controlled SQL only; it never interpolates request data.
- The HTTP server sets read-header and idle timeouts and a maximum header size before any product route exists.

## Out of scope

Any product domain. Any user, session, or repository table. Any frontend code. Authentication and authorization. Deploy packaging beyond the local development recipe (see [../architecture/packaging.md](../architecture/packaging.md)).

## Exit criteria

A contributor can follow the README on a clean machine, start a PostgreSQL instance without installing it globally, apply migrations twice with the second run a no-op, force a failing migration and see both its schema work and ledger entry roll back, start the service, observe `/api/health` and `/api/ready`, stop it with an interrupt, and see it exit within the configured deadline. CI runs formatting, vet, build, and race tests with a real database. No product feature exists yet, and no placeholder package has been created for one.
