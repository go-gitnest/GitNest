# GitNest

GitNest is an open-source, self-hosted Git forge built for feature parity with GitHub. The target is one installable instance that covers repositories, issues, pull requests, review, CI, packages, releases, search, administration, and hosted development workspaces.

> **Status: no implementation exists yet.** The repository currently contains documentation only. The code was removed so that it can be rebuilt from these documents in a defined order, starting with [phase 0](docs/roadmap/00-service-foundation.md). The commands in "Run locally" and "First administrator" below describe target behaviour and work once phases 0 and 1 are complete. Start with [AGENTS.md](AGENTS.md).

The decided exclusions are the mobile app, GitHub's own Codespaces, Advanced Security (code scanning, secret scanning, Dependabot), Gist, payments, and AI code features. See [docs/product-goals.md](docs/product-goals.md).

## Documentation

| Document | Contents |
| --- | --- |
| [AGENTS.md](AGENTS.md) | How to pick up work, verification commands, hard rules |
| [docs/product-goals.md](docs/product-goals.md) | Positioning, the parity definition, decided exclusions, definition of done |
| [docs/architecture.md](docs/architecture.md) | Module boundaries, storage, security model, runtime topology |
| [docs/implementation-plan.md](docs/implementation-plan.md) | Phase order, current status, immediate next work |
| [docs/roadmap/](docs/roadmap) | Per-phase workstreams and acceptance criteria |
| [docs/architecture/](docs/architecture) | Cross-cutting design: configuration, secrets, data model, frontend, testing, performance, URLs, administration, packaging |
| [docs/security/threat-model.md](docs/security/threat-model.md) | Per-surface threats, required mitigations, test obligations |
| [docs/reference/events.md](docs/reference/events.md) | Audit and webhook event catalogues |
| [docs/parity-checklist.md](docs/parity-checklist.md) | The auditable GitHub capability inventory |
| [docs/glossary.md](docs/glossary.md) | Shared vocabulary |

## Requirements

- Go 1.26 or newer
- PostgreSQL 14 or newer, or Docker to run the development database in `compose.yaml`
- Node.js is not needed yet; the React app has not been added

## Local PostgreSQL

`compose.yaml` runs a development PostgreSQL with the user, password, and database that the default `GITNEST_DATABASE_URL` already expects:

```sh
docker compose up -d postgres   # start it in the background
docker compose ps               # wait until the service reports healthy
docker compose down             # stop it, keeping the data volume
docker compose down -v          # stop it and delete all local database data
```

The data lives in the `gitnest-postgres` volume, not in the repository. If you already run PostgreSQL 14 or newer somewhere, point `GITNEST_DATABASE_URL` at it and skip this section.

## Run locally

Create a PostgreSQL database and configure its URL. The default is intended for a local development database:

```powershell
$env:GITNEST_DATABASE_URL = "postgres://gitnest:gitnest@localhost:5432/gitnest?sslmode=disable"
```

Apply the schema, then start the server:

```sh
go run ./cmd/gitnest migrate
go run ./cmd/gitnest serve
```

The server listens on `0.0.0.0:8080` by default. It does not run migrations automatically. Set `GITNEST_HOST=127.0.0.1` to keep a development instance local.

Check its endpoints:

```sh
curl http://localhost:8080/api/health
curl http://localhost:8080/api/ready
```

`/api/health` reports that the process is alive. `/api/ready` checks PostgreSQL and returns `503` if the database is unavailable.

## First administrator

GitNest has no remote setup page. After applying migrations on a fresh database, create the first administrator from a terminal on the server:

```sh
go run ./cmd/gitnest admin create
```

The command asks for a username, an email address, and a password. The password is read without echo, is never accepted as a command-line argument, and is stored only as an Argon2id hash that records the parameters used to create it. The command refuses to run when the instance already has an administrator, reports when the database has not been migrated, and returns a non-zero exit status on every failure. Only this one account is created from the terminal; it adds everyone else.

## Reverse proxy and TLS

Terminate TLS at a reverse proxy and forward to `GITNEST_HOST:GITNEST_PORT`. Set `GITNEST_PUBLIC_URL` to the address users will type:

```sh
GITNEST_PUBLIC_URL=https://git.example.com
```

GitNest builds clone URLs and redirects from `GITNEST_PUBLIC_URL` alone. It does not read the request `Host`, `X-Forwarded-Host`, or `X-Forwarded-Proto` headers, so an unexpected `Host` header cannot change a generated URL. A non-loopback value must use `https`; plain `http` is accepted only for `localhost` and loopback addresses so a development instance stays convenient. Startup fails with a clear error when the value is not an absolute URL without a path, credentials, query, or fragment.

## Configuration

All settings are optional. The table shows their defaults.

| Variable | Default | Purpose |
| --- | --- | --- |
| `GITNEST_HOST` | `0.0.0.0` | HTTP bind address |
| `GITNEST_PORT` | `8080` | HTTP port |
| `GITNEST_PUBLIC_URL` | `http://localhost:8080` | Canonical URL for clone URLs and redirects; `https` unless it points at loopback |
| `GITNEST_DATABASE_URL` | `postgres://gitnest:gitnest@localhost:5432/gitnest?sslmode=disable` | PostgreSQL connection URL |
| `GITNEST_REPO_ROOT` | `./repositories` | Repository storage root for later milestones |
| `GITNEST_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, or `error` |
| `GITNEST_SSH_LISTEN` | `:2222` | Reserved SSH listen address |
| `GITNEST_READ_HEADER_TIMEOUT` | `5` | HTTP read-header timeout in seconds |
| `GITNEST_SHUTDOWN_TIMEOUT` | `10` | Graceful shutdown timeout in seconds |

The database URL contains credentials. Keep it in the environment or a secret manager, not in source control or logs.

## Development

```sh
go test ./...
go vet ./...
go build ./...
```

The same checks are available through `make test`, `make vet`, and `make build`.

Tests that need PostgreSQL skip themselves unless `GITNEST_TEST_DATABASE_URL` names a maintenance connection. Each test creates and drops its own schema, so they never touch application data and do not need a database of their own:

```powershell
$env:GITNEST_TEST_DATABASE_URL = "postgres://gitnest:gitnest@localhost:5432/gitnest?sslmode=disable"
go test -race ./...
```

`.github/workflows/ci.yml` runs the formatting check, `go vet ./...`, `go build ./...`, and `go test -race ./...` against a PostgreSQL service container. It is created by the phase 0 workstreams.

## Project layout

Target layout. Only the documentation exists today; every path below is created by the phase that owns it.

```text
cmd/gitnest/                 Service entry point and command-line commands
internal/app/                Startup, HTTP server, shutdown, and command wiring
internal/config/             Environment configuration and validation
internal/identity/           Accounts, passwords, sessions, invitations, administrator bootstrap
internal/platform/postgres/  PostgreSQL connection, migrations, and test helpers
internal/transport/http/api/ Health and readiness endpoints
migrations/                  Embedded SQL schema migrations
compose.yaml                 Local development PostgreSQL
.github/workflows/ci.yml     Formatting, vet, build, and test checks
docs/                        Product goals, architecture, implementation plan
docs/roadmap/                Per-phase workstreams and acceptance criteria
docs/architecture/           Cross-cutting design documents
docs/design/                 Per-workstream design notes
AGENTS.md                    How to pick up work, verification commands, hard rules
```

See [docs/architecture.md](docs/architecture.md) for the intended architecture and longer-term plan.

The product scope and release goals are in [docs/product-goals.md](docs/product-goals.md). Work is staged in [docs/implementation-plan.md](docs/implementation-plan.md), with per-phase detail under [docs/roadmap/](docs/roadmap).
