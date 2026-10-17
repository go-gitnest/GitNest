# GitNest

GitNest is an open-source, self-hosted Git forge built for feature parity with GitHub. The target is one installable instance that covers repositories, issues, pull requests, review, CI, packages, releases, search, administration, and hosted development workspaces.

> **Status: phase 0 complete, phase 1 in progress.** The service foundation is in place and the first product feature has landed. These commands work today:
>
> | Command | State |
> | --- | --- |
> | `gitnest version` | Prints the build identity |
> | `gitnest migrate` | Applies the embedded migrations, recording each in a ledger |
> | `gitnest serve` | Serves `/api/health` (liveness) and `/api/ready` (readiness, `503` when PostgreSQL is unreachable) |
> | `gitnest admin create` | Creates the instance's first administrator, reading the password without echo |
>
> The account HTTP surface exists under `/api/v1`: `POST /login`, `POST /logout`, `GET /me`, `POST /user/invitations` (administrator only), and `POST /invitations/accept` (the invitation secret goes in an `X-Invitation-Token` header). Sign-out and invitation creation require the `X-CSRF-Token` value returned by `POST /login`, because they change state.
>
> The repository read API is under `/api/v1` too: `GET`/`POST /user/repos` for the caller's own repositories, and `GET /repos/{owner}/{repo}` with `contents/{path}` (a file, or a directory listing when the path is a directory) and `commits`. Anonymous callers may read a public repository; a private one answers `404`, indistinguishable from a repository that does not exist.
>
> The read surfaces are styled from a stylesheet embedded in the binary, with light and dark themes driven by one token set that the interactive surfaces share; a test asserts the two stylesheets'' token values agree, and the contrast ratio of every foreground colour is computed and checked against WCAG AA in both themes. It is embedded rather than built by Vite, so the read pages keep working on a checkout where the frontend has not been built.
>
> The same content is readable in a browser without JavaScript, because the read surfaces are server-rendered: `/alice/my-repo` for the overview, `/alice/my-repo/tree/main` for a directory, `/alice/my-repo/blob/main/README.md` for a file, and `/alice/my-repo/commits/main` for history. Repository content is always escaped text — Markdown rendering and syntax highlighting are not implemented yet.
>
> Repository settings are under `/api/v1` as well: `PATCH /repos/{owner}/{repo}` changes the visibility, the default branch, or the archive state, and `/repos/{owner}/{repo}/collaborators` lists, grants, and revokes direct access. Making a repository public requires its own name as the `confirmation` field; making it private requires `change-visibility`. Every change writes an append-only audit record in the same transaction, and archiving makes a repository read-only for content: a `git push` is refused with a message naming the archive state while a fetch still works.
>
> Git clients authenticate with a personal access token, issued once and shown once. The value is stored as a SHA-256 digest, so it cannot be recovered afterwards; a token is revoked rather than deleted, and a revoked token fails on the next request.
>
> An **anonymous** request is answered with a `401` challenge rather than a `404`, because Git presents no credential until it is asked for one — without the challenge a token would never be sent. A request that presents a credential this server rejects is answered with the same not-found shape a nonexistent repository gets, so a caller holding a valid credential cannot use a refusal to learn whether a repository exists.
>
> Backup and restore are CLI-only, because they need local access and the connection string must not appear on an argument vector:
>
> ```sh
> gitnest backup --target /srv/backups/gitnest-2026-10-14   # capture and verify
> gitnest backup-verify --source /srv/backups/gitnest-2026-10-14
> gitnest restore --source /srv/backups/gitnest-2026-10-14 --confirm   # stop the server first
> ```
>
> A recovery set is one directory holding the PostgreSQL dump, the repository storage — including LFS objects, which live under the repository root — and a manifest that makes the set verifiable rather than merely present. The procedure, its limits, and the restore drill are in [docs/operations/backup-restore.md](docs/operations/backup-restore.md). The instance secret key is deliberately not part of a set.
>
> Git LFS is served from the same origin as the Git transport: `POST /{owner}/{repo}.git/info/lfs/objects/batch` negotiates, and `.../lfs/objects/{oid}` uploads and downloads. Objects are addressed by content hash, stored under the instance's repository root, and verified against the declared digest and size before they become reachable; the per-object cap is `git.lfs_max_object_bytes` (2 GiB) and the per-repository allowance is `git.lfs_repository_bytes` (50 GiB). The batch and object URLs are built from `GITNEST_PUBLIC_URL`, so reverse proxies work.
>
> The interactive surfaces are a React application under `web/`, built by Vite into `web/dist` and embedded into the binary — so a deployed instance needs no Node.js runtime. Repository creation is at `/new` and repository settings at `/{owner}/{repo}/settings`; both are mounted by the Go process and filled by the bundle, so a deep link works on first load. A page load issues its own CSRF token, so reloading does not lose the ability to submit a form; issuing a token replaces the session's previous one, so if two documents are open from one session the later one's token is the valid one. **Building the frontend is required before the interactive surfaces work:** run `npm install && npm run build` in `web/`, then rebuild the server. Without a build, `go build` and the whole test suite still pass and the read surfaces still work; `/new` reports the commands to run.
>
> ```sh
> cd web && npm install && npm run build && cd .. && go build ./...
> ```
>
> `npm run typecheck` and `npm run lint:strings` check the client, and `npm test` runs its suite; the string check is the CI gate that fails on a hardcoded user-visible string.
>
> Public repositories can be cloned and fetched over smart HTTP with a real Git client: `git clone http://<host>/<owner>/<repo>.git`. Clone, fetch, and push all resolve every caller as anonymous, because the credential model that turns an `Authorization` header into an identity arrives in phase 4 — so a private repository is not reachable over Git yet, and **push is enforced but unusable in practice until that model exists**. Anonymous callers are refused a push exactly as they are refused a private repository.
>
> Everything else below describes target behaviour and arrives with its phase. To work on the project, start with [CONTRIBUTING.md](CONTRIBUTING.md); for what the product is read [docs/product-goals.md](docs/product-goals.md), and for how it is put together read [docs/architecture.md](docs/architecture.md). What is built, what is next, and what is still missing are in [docs/implementation-plan.md](docs/implementation-plan.md).

The decided exclusions are the mobile app, GitHub's own Codespaces, Advanced Security (code scanning, secret scanning, Dependabot), Gist, payments, and AI code features. See [docs/product-goals.md](docs/product-goals.md).

## Documentation

| Document | Contents |
| --- | --- |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to pick up work, code conventions, the non-negotiables, definition of done |
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

`/api/health` reports that the process is alive, independently of its dependencies. `/api/ready` checks PostgreSQL and returns `503` when the database is unavailable; neither response carries a connection string, a database error, or configuration. Both include the build version so a probe identifies the release.

The build identity is also available directly:

```sh
go run ./cmd/gitnest version
```

Stop the server with an interrupt (`Ctrl+C`) or a termination signal. It stops accepting new connections, lets in-flight requests finish, closes the database pool, and exits — all within `GITNEST_SHUTDOWN_TIMEOUT`. A second signal does not wait: it terminates the process immediately. A clean stop exits `0`; a stop that misses the deadline exits non-zero, so a supervisor can tell the two apart.

## Logs

Logs are one JSON object per line on standard error, at the level `GITNEST_LOG_LEVEL` selects. Command results — a version string, a migration summary — stay on standard output as data, so piping them into another tool does not have to filter log lines out.

Every request line carries a correlation identifier, taken from the `X-Request-Id` header when the client sends a well-formed one and generated otherwise; the request line and anything the handler logs share it. Request query strings are never logged, because they can carry a token, and registered secret values are redacted before a record is written, in their plain, base64, and JSON-escaped forms.

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

These are the startup settings, read once when the process starts. All of them are optional; the table shows their defaults. Policy that an operator changes at runtime is not configured here — it lives in the database and is edited in the operator console.

| Variable | Default | Purpose |
| --- | --- | --- |
| `GITNEST_HOST` | `0.0.0.0` | HTTP bind address |
| `GITNEST_PORT` | `8080` | HTTP port |
| `GITNEST_PUBLIC_URL` | `http://localhost:8080` | Canonical URL for clone URLs and redirects; `https` unless it points at loopback |
| `GITNEST_DATABASE_URL` | `postgres://gitnest:gitnest@localhost:5432/gitnest?sslmode=disable` | PostgreSQL connection URL |
| `GITNEST_DATA_DIR` | `./data` | Writable root for instance data; one mounted volume in a container deployment |
| `GITNEST_REPO_ROOT` | `<GITNEST_DATA_DIR>/repositories` | Bare repository storage |
| `GITNEST_TMP_ROOT` | `<GITNEST_DATA_DIR>/tmp` | Scratch space for Git operations, cleaned at startup. It must not contain the repository root, or live inside it |
| `GITNEST_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, or `error` |
| `GITNEST_SSH_LISTEN` | `:2222` | Git-over-SSH listen address, unused until the SSH transport lands |
| `GITNEST_READ_HEADER_TIMEOUT` | `5s` | HTTP read-header timeout, a duration such as `5s` or `1m` |
| `GITNEST_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown deadline, a duration |
| `GITNEST_DB_MAX_OPEN_CONNS` | `20` | Maximum open connections per process. The worker run mode uses `5` so it cannot crowd out request handling |
| `GITNEST_DB_MAX_IDLE_CONNS` | `5` | Idle connections kept for reuse; must not exceed the maximum open connections |
| `GITNEST_DB_CONN_MAX_LIFETIME` | `30m` | Retire a connection after this long, so a restart or failover is survived |
| `GITNEST_DB_CONN_MAX_IDLE_TIME` | `5m` | Close a connection that has been idle this long |
| `GITNEST_DB_ACQUIRE_TIMEOUT` | `2s` | How long a request waits for a connection, and how long a readiness check may take |

A variable that is set but empty is an error rather than a silent fallback to the default, and every problem is reported at once with the name of the variable to fix. Startup refuses to run until the configuration is valid.

The database URL contains credentials. Keep it in the environment or a secret manager, not in source control or logs.

## Deployment

A conventional deployment is one host, one binary, a systemd unit, PostgreSQL on the same host or reachable by DSN, and a data directory outside the repository tree. [`deploy/`](deploy/README.md) holds the unit, an environment example, and the container image; [`deploy/README.md`](deploy/README.md) walks through installing them.

```sh
sudo useradd --system --create-home --home-dir /var/lib/gitnest --shell /usr/sbin/nologin gitnest
sudo install -m 0755 gitnest /usr/local/bin/gitnest
sudo install -d -o root -g gitnest -m 0750 /etc/gitnest
sudo install -o root -g gitnest -m 0640 deploy/gitnest.env.example /etc/gitnest/gitnest.env
sudo install -m 0644 deploy/gitnest.service /etc/systemd/system/
sudo -e /etc/gitnest/gitnest.env      # replace every placeholder
sudo -u gitnest gitnest migrate
sudo -u gitnest gitnest admin create --username admin --email admin@example.com
sudo systemctl enable --now gitnest.service
```

The unit uses `Type=notify`, which means the binary reports readiness to systemd **only after it has confirmed the database is reachable** — so a process that is listening but cannot serve is never given traffic. Readiness is what `/api/ready` answers, and the two cannot disagree.

The image is built by the [`Dockerfile`](Dockerfile), whose runtime stage contains the binary, Git, and CA certificates and nothing else: Node.js is a build-time dependency and is absent from it.

```sh
docker build -t gitnest .
docker run --rm -p 8080:8080 \
  -e GITNEST_PUBLIC_URL=http://localhost:8080 \
  -e GITNEST_DATABASE_URL=postgres://gitnest:gitnest@host.docker.internal:5432/gitnest?sslmode=disable \
  -v gitnest-data:/var/lib/gitnest \
  gitnest migrate
```

## Health and readiness

```sh
gitnest health            # probes /api/health, exits non-zero when the process is not alive
gitnest health --ready    # probes /api/ready, which reports database availability
```

Both accept `--url` to probe somewhere other than the configured listener, and both are what a scheduler or load balancer should use: `--ready` withholds traffic until the database is reachable, which `/api/health` does not promise. The container's `HEALTHCHECK` uses this command rather than `curl`, because the image deliberately does not carry it.

## Upgrading

```sh
gitnest backup --target /srv/backups/gitnest-$(date +%F)   # mandatory before a schema change
gitnest backup-verify --source /srv/backups/gitnest-$(date +%F)
gitnest migrate --dry-run                                  # read-only preflight
gitnest migrate                                            # applies, under the migration lock
```

`gitnest migrate --dry-run` lists what would be applied and **exits non-zero when the database is ahead of the binary** — the state a rollback to an older binary produces, where continuing would run code against a schema it was not written for. It changes nothing, so it is safe against production and safe in a unit's `ExecStartPre`; the migration itself is not, and needs the operator present with a verified backup behind it. The full procedure, including the order to restart units in, is in [`deploy/README.md`](deploy/README.md) and [docs/architecture/packaging.md](docs/architecture/packaging.md) section 11.

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

| Variable | Effect |
| --- | --- |
| `GITNEST_TEST_DATABASE_URL` | Maintenance connection used by every database-backed test; without it those tests skip |
| `GITNEST_TEST_REQUIRE` | Comma-separated capabilities that must be present, for example `postgres`. A test that would skip because of a missing capability fails instead, so a misconfigured CI job cannot report success while running nothing |
| `GITNEST_TEST_DB_CONCURRENCY` | Caps how many test schemas one process holds at once, so a parallel suite stays inside the server's connection limit |
| `GITNEST_TEST_GIT_BIN` | Git executable the Git runner tests drive, default `git` from `PATH`. Without a Git executable those tests skip, unless `GITNEST_TEST_REQUIRE` names `git` |

Each test gets a uniquely named schema with the shipped migrations applied, drops it when it finishes — including when it fails — and never touches the maintenance database's own tables.

`.github/workflows/ci.yml` runs five jobs on every push and pull request: `go-format-vet` (formatting and `go vet ./...`), `go-build`, `go-test-unit` (the suite with no infrastructure, where database-backed tests skip), `go-test-integration` (the same suite against a PostgreSQL service container, with `GITNEST_TEST_REQUIRE=postgres` so a missing database fails instead of skipping), and `compose-postgres` (starts the development recipe and connects with the documented credentials).

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
CONTRIBUTING.md              How to pick up work, conventions, the non-negotiables
```

See [docs/architecture.md](docs/architecture.md) for the intended architecture and longer-term plan.

The product scope and release goals are in [docs/product-goals.md](docs/product-goals.md). Work is staged in [docs/implementation-plan.md](docs/implementation-plan.md), with per-phase detail under [docs/roadmap/](docs/roadmap).
