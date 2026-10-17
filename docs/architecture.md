# GitNest Architecture

**Status:** Target architecture. Nothing is implemented yet; the implementation is built from these documents.
**Scope:** System structure, module boundaries, storage, security boundaries, and delivery sequence.
**Related documents:** [product-goals.md](product-goals.md) for the scope and the parity definition, [implementation-plan.md](implementation-plan.md) for the phase order and current status, the per-phase workstreams under [roadmap/](roadmap/), and the design documents [configuration.md](architecture/configuration.md), [secrets.md](architecture/secrets.md), [frontend.md](architecture/frontend.md), [data-model.md](architecture/data-model.md), [testing.md](architecture/testing.md), [performance.md](architecture/performance.md), [urls-and-identifiers.md](architecture/urls-and-identifiers.md), [administration.md](architecture/administration.md), [packaging.md](architecture/packaging.md), [events.md](reference/events.md), [threat-model.md](security/threat-model.md), [glossary.md](glossary.md), and [parity-checklist.md](parity-checklist.md).

## 1. Product and design goals

GitNest is a self-hosted Git forge whose reference implementation is GitHub: the same concepts, workflows, permission model, and API shapes, delivered in phases. The product rationale, the parity definition, and the decided exclusions (mobile app, GitHub's own Codespaces, Advanced Security, Gist, payments, AI features) are in [product-goals.md](product-goals.md). The staged plan and its exit criteria are in [implementation-plan.md](implementation-plan.md).

The architecture therefore has to satisfy two constraints that pull in opposite directions:

1. **Parity needs breadth.** Issues, pull requests, review, search, CI, packages, releases, pages, wikis, discussions, projects, organizations, enterprise administration, and a compatible REST and GraphQL API all have to exist and agree with each other.
2. **A self-hosted install has to stay runnable by one operator.** The default deployment is one binary, PostgreSQL, and a data directory. Everything heavier — object storage, a search service, a broker, extra nodes, a container runtime — is optional and must degrade gracefully.

The resolution is a modular monolith with optional components: one codebase, one database, one deployment unit for the common case, with interfaces at the boundaries where a large instance substitutes a heavier implementation.

The architecture prioritizes:

- A straightforward single-server install and upgrade path.
- Explicit security boundaries for untrusted input, especially Git operations and untrusted code execution.
- Stable internal IDs separated from names, URLs, and filesystem paths.
- Standard Git repositories on disk, so backup and recovery stay practical.
- Contracts (authorization, storage, API shapes) that many features share, so parity work does not fork them.

## 2. Architecture decision

A **modular monolith written in Go**, deployed as one binary with several run modes:

- The **web** process serves the browser UI, the REST and GraphQL APIs, webhook delivery, package registries, and Git-over-HTTP.
- The **worker** process runs durable background jobs: notifications, email, indexing, diff refresh, retention, cleanup.
- The **SSH** entry point authenticates Git-over-SSH sessions and dispatches Git operations through the same authorization path as HTTP.
- The **runner** process executes CI jobs. It is a separate trust domain: it never shares a process, credentials, or filesystem view with the web or worker processes.
- The **workspace** agent (phase 9) manages cloud-development containers, also a separate trust domain.

Module boundaries are explicit in code, and dependencies flow inward toward domain rules: transports and adapters depend on domain services, never the reverse.

Do not split into microservices for their own sake. Split only where the trust boundary demands it (runners, workspaces) or where measured load requires it (phase 10).

## 3. System context

```text
  Browser ------------------------------------------------------------------+
                                                                            |
  REST / GraphQL API clients -----------------------------------------------|
                                                                            v
  Git client (HTTPS) ----------> HTTPS reverse proxy ------------> GitNest web process ---> PostgreSQL
  Git client (SSH) -------------> GitNest SSH entry point ------>      |                     |
  Package client (oci/npm/...) -> GitNest web process ---------->      |                     |
                                                                            |                 |
  Webhook consumers <----------- outbound delivery <-----------------------+
                                                                            |
  GitNest worker process <--------------------------------------------- (durable jobs)
                                                                            |
  GitNest runner process (CI jobs, untrusted) <------------------------ (scheduler)
  GitNest workspace agent (cloud development, untrusted) <-------------- (scheduler)
                                                                            |
  repository storage (filesystem) <-----------------------------------------+
  object storage (optional: LFS, artifacts, package blobs, pages) <---------+
  search service (optional: code search index) <----------------------------+
```

The reverse proxy terminates TLS and forwards HTTP. GitNest owns authentication and authorization; the proxy is not an authorization boundary. Public URLs are built from the configured `GITNEST_PUBLIC_URL`, never from request or forwarded headers.

Runners and workspace agents connect to the control plane **outbound** and receive jobs; the control plane never gives them credentials that reach the database.

## 4. Application modules

| Module | Responsibility |
|---|---|
| `identity` | Accounts, password and session authentication, two-factor authentication, personal access tokens, SSH and GPG keys, invitations |
| `organization` | Organizations, enterprises, teams, membership, roles, SSO/SCIM bindings |
| `repository` | Repository metadata, visibility, permissions, collaborators, branch protection, rulesets, deploy keys, topics, stars and watching |
| `gittransport` | Shared Git authorization and dispatch for HTTP and SSH, protocol handling, LFS, archives |
| `issues` | Issues, comments, labels, milestones, assignees, templates, timeline |
| `pullrequest` | Pull requests, diffs, reviews, review threads, merge orchestration, merge queue |
| `projects` | Project boards, tables, roadmaps, custom fields, views, automations |
| `discussions` | Discussions, categories, answers, polls |
| `pages` | Static site hosting, builds, custom domains |
| `search` | Query parsing, indexing, and ranked retrieval for repositories, code, issues, commits, and users |
| `actions` | Workflow parsing, expression evaluation, runs, jobs, artifacts, caches, secrets, environments |
| `runner` | Runner registration, job scheduling, execution isolation, log streaming |
| `packages` | Registry protocols (OCI, npm, Maven, NuGet, RubyGems, Composer, Helm, generic), permissions, retention |
| `webhooks` | Event payloads, subscriptions, signing, delivery, retries, delivery log |
| `authapp` | OAuth apps and GitHub Apps: registration, installations, tokens, permissions |
| `activity` | Notifications, subscriptions, audit log, activity timeline |
| `workspace` | Cloud-development workspace lifecycle, images, volumes, port forwarding, quotas |
| `jobs` | Durable queue, claiming, retries, backoff, terminal failure records |
| `api` / `web` | REST and GraphQL request handling, HTML pages, validation, response mapping |
| `storage` | Interfaces and implementations for Git repositories, uploads, artifacts, and object storage |

Modules call each other through application services or narrow interfaces, never by reaching into another module's tables. HTTP handlers stay thin: parse, validate, call a service, serialize the result.

### 4.1 Repository layout

Create feature packages when their first workstream starts; do not add placeholder packages.

```text
.
├── cmd/
│   ├── gitnest/                    # CLI entry point: serve, worker, migrate, admin, ssh
│   └── gitnest-runner/             # Runner agent (separate trust domain)
├── internal/
│   ├── app/                        # Dependency wiring and lifecycle for each run mode
│   ├── config/                     # Configuration loading and validation
│   ├── identity/                   # Accounts, credentials, sessions, tokens, keys, 2FA
│   ├── organization/               # Organizations, enterprises, teams, membership
│   ├── repository/                 # Repository metadata, permissions, protection, rulesets
│   ├── issues/                     # Issues, comments, labels, milestones
│   ├── pullrequest/                # Pull requests, reviews, merge
│   ├── projects/                   # Project boards and custom fields
│   ├── discussions/                # Discussions and categories
│   ├── pages/                      # Static hosting and builds
│   ├── search/                     # Indexing and query handling
│   ├── actions/                    # Workflow model, runs, artifacts, caches, secrets
│   ├── runner/                     # Scheduler, runner registry, job execution
│   ├── packages/                   # Registry protocol implementations
│   ├── webhooks/                   # Event delivery and subscriptions
│   ├── authapp/                    # OAuth apps and GitHub Apps
│   ├── activity/                   # Notifications, audit, timeline
│   ├── workspace/                  # Cloud-development workspaces
│   ├── jobs/                       # Durable queue and worker handlers
│   ├── gittransport/               # Shared Git service authorization and dispatch
│   ├── transport/
│   │   ├── http/
│   │   │   ├── web/                # HTML routes and handlers
│   │   │   ├── api/                # REST and GraphQL routes and handlers
│   │   │   └── git/                # Git smart HTTP endpoints
│   │   └── ssh/                    # Git-over-SSH authentication and dispatch
│   └── platform/
│       ├── postgres/               # Database connection and repositories
│       ├── git/                    # Safe Git executable invocation
│       ├── storage/                # Repository, upload, and object storage
│       ├── cache/                  # In-process then optional shared cache
│       ├── search/                 # Search backend adapters
│       ├── container/              # Container runtime adapters for runners and workspaces
│       ├── mail/                   # Email adapter
│       └── logging/                # Logging setup
├── migrations/                     # Ordered forward-only SQL migrations
├── web/                            # React application (src) and built assets (dist)
├── docs/                           # Architecture, product goals, plan, roadmap
│   └── roadmap/                    # Per-phase workstreams and acceptance criteria
├── deploy/                         # Dockerfile, examples, system-service units
├── scripts/                        # Development and release helper scripts
├── .github/workflows/              # CI for GitNest itself
├── compose.yaml                    # Local development PostgreSQL
├── go.mod
└── README.md
```

### 4.2 Package rules

- `cmd/*` only parses commands and starts a run mode. Business logic lives in `internal/`.
- `internal/app` constructs dependencies and owns startup and shutdown. Feature packages must not import HTTP handlers.
- Feature packages own their domain types, application services, and persistence-facing interfaces. Keep feature-specific SQL beside the feature unless sharing is justified.
- `internal/platform` contains adapters for external systems and must not decide product permissions or domain behaviour.
- `internal/transport` maps protocols to application services. Every transport must call the same authorization service for Git and repository operations.
- Keep the React source under `web/`. Vite serves it in development and proxies API requests to Go; a release serves built assets from the Go process so no Node.js runtime is needed in production.
- Schema changes go in `migrations/` as a new forward-only file. Never edit a migration that may have been applied.
- Tests live beside the code as `*_test.go`. Unit tests cover domain behaviour; integration tests cover the PostgreSQL, Git, storage, and execution boundaries and are gated by an environment DSN.
- Only packages imported by external modules belong in a future `pkg/`. Keep the application API under `internal/`.

## 5. Technology baseline

- **Language:** Go 1.26 or newer, standard library first. The HTTP layer uses `net/http` with method-aware `ServeMux` patterns.
- **Database:** PostgreSQL, the system of record for all structured data, durable jobs, and the search fallback. Forward-only SQL migrations checked into the repository.
- **Git execution:** the installed Git executable, invoked with argument arrays, bounded contexts, a controlled environment, and no shell interpolation.
- **Repository storage:** local filesystem with a configurable root, addressed by internal UUID rather than user-supplied names. An object-storage backend is optional for LFS, artifacts, package blobs, and pages.
- **Web UI:** React with TypeScript, a same-origin client of the Go API. Vite in development; built assets shipped with the release.
- **APIs:** a versioned REST API with GitHub-compatible shapes, plus GraphQL for the surface that warrants it.
- **Untrusted execution:** a container runtime with strict CPU, memory, disk, time, and network limits, no host credentials, and deterministic cleanup.
- **Deployment:** one binary, PostgreSQL, Git, and a writable data directory; a container image and a conventional system-service deployment, with optional worker, runner, and workspace agents.

Use interfaces at real infrastructure boundaries (database, storage, cache, search, container runtime, mail, job execution), not as a general abstraction around every function.

## 6. Runtime topology

The common case is a single node:

```text
gitnest serve        # web UI, API, Git over HTTP, package registries, webhook delivery
gitnest worker       # background jobs
gitnest ssh          # Git over SSH
gitnest-runner       # CI jobs (may run on the same host, but in its own trust domain)
```

Every mode is a flag or subcommand of the same binary except the runner agent, which is packaged separately because it is expected to run on other hosts.

Growth path, in order of when it is likely to be needed:

1. **Separate worker from web** so background work cannot starve request handling.
2. **Move runners to separate hosts** and register them with labels.
3. **Object storage** for LFS, artifacts, package blobs, and pages.
4. **A search service** for code search at scale, with SQL fallback.
5. **Multiple web nodes** behind the proxy, sharing PostgreSQL and repository storage. This requires a locking and single-writer design; mounting the same directory on two nodes is not sufficient.
6. **Read replicas** for read-heavy surfaces.

Nothing in this list may become a requirement for the single-node install.

## 7. Data and storage boundaries

**PostgreSQL** stores accounts, credentials, sessions, tokens, keys, organizations and teams, repository metadata, permissions and protection rules, issues, pull requests, reviews, comments, projects, discussions, notifications, audit events, webhooks and their deliveries, actions runs and their metadata, package metadata, workspace records, and durable job records.

**Repository storage (filesystem)** holds bare Git repositories and temporary Git operation data in a dedicated, cleaned-up directory.

**Object storage (optional)** holds LFS objects, release assets, action artifacts and caches, package blobs, page builds, and user uploads when the local filesystem is no longer appropriate.

**Search index (optional)** holds the code and text index. PostgreSQL full-text search is the fallback for small instances.

**Container runtime** holds nothing durable. Runner job containers and workspace containers are disposable; workspace *volumes* are the exception and are explicitly persisted and quota-managed.

Rules:

- Repository and storage paths derive from internal IDs, never from user-supplied names. Rename updates metadata and moves storage through a controlled operation.
- Database and repository-storage backups are one recovery set with one documented procedure. Database transactions cannot make filesystem moves atomic, so any cross-boundary operation needs an idempotent repair path.
- Losing the search index or the cache must never lose data; both are rebuildable from PostgreSQL and repository storage.
- Secrets (tokens, keys, webhook secrets, action secrets) are stored encrypted or hashed as appropriate and are never included in backups in plaintext beyond what the operator's own key management provides.

## 8. Git operation flows

### Clone, fetch, and archive over HTTPS

1. Resolve the repository route and authenticate when the repository is private.
2. Check read permission in the shared repository service before starting any Git process.
3. Resolve the canonical repository path from its internal ID.
4. Dispatch the smart HTTP request (or protocol v2, or an archive request) with a minimal, controlled environment.
5. Stream the response while enforcing request, process, and resource limits.

### Push over HTTPS or SSH

1. Authenticate the user, token, or SSH key and resolve the requested repository.
2. Check write permission before accepting the operation.
3. Enforce branch protection and rulesets: protected refs, required status checks, required reviews, and force-push policy.
4. Start a bounded `receive-pack` operation for the canonical repository path.
5. Validate the resulting ref updates, record the outcome, and emit audit and activity events.
6. Enqueue follow-up work: notifications, search indexing, pull-request refresh.

Git work is an untrusted-input boundary. Never build shell strings from request data. Restrict the allowed Git service commands, apply time and resource limits, and clean up temporary data after success, failure, or cancellation.

### Git LFS

LFS requests are authenticated and authorized through the same repository service as Git operations, and object storage is addressed by a content hash rather than a user-supplied path.

## 9. Collaboration flows

- **Issue and pull-request writes** go through their domain services, which check repository access and emit activity events.
- **Pull-request merge** rechecks permission and the current base and head refs immediately before updating refs, and uses ref transactions or compare-and-swap so a concurrent push cannot silently overwrite a newer base. The ref update and the database state transition are ordered so a crash is recoverable.
- **Notifications** are written to a durable outbox in the same transaction as the domain change, then delivered by a worker. Retrying delivery must not create duplicate user-visible notifications.
- **Search indexing** is asynchronous and idempotent; an index lag must never change authorization outcomes.
- **Webhook delivery** is durable, signed, retried with bounded backoff, and recorded in a delivery log.

## 10. Untrusted execution boundaries

Actions jobs and cloud-development workspaces execute user-controlled code. They share a design rule: **the control plane never hands them a credential that reaches GitNest's own data.**

```text
  scheduler (control plane)  ---> job assignment (short-lived, scoped token)
        |
        v
  runner / workspace agent (separate host or process, no DB credentials)
        |
        v
  container (untrusted workload: CPU/memory/disk/time/network limits)
        |
        +--> repository clone over HTTPS using a scoped, expiring token
        +--> artifacts and caches over the authenticated API
        +--> logs streamed back and scrubbed of registered secrets
```

Requirements for both:

- No access to the GitNest database, its configuration, or other tenants' storage.
- A scoped, short-lived credential for the specific repository and job; never a user's personal token.
- Enforced CPU, memory, disk, process, and wall-clock limits; no unbounded network egress (configurable allow-list).
- Deterministic cleanup on success, failure, cancellation, and agent crash.
- Secrets are injected into the job environment, registered for log masking, and never persisted in the workspace image.
- The web process must never execute workflow or workspace code, directly or transitively.

## 11. API design principles

- **Versioned and stable.** REST under a versioned prefix with an API version header; breaking changes only in a new version with a deprecation window.
- **GitHub-compatible shapes.** Resource names, fields, pagination (`per_page`, `page`, `Link` headers), conditional requests (`ETag`, `If-None-Match`), rate-limit headers, and error bodies follow GitHub conventions so existing clients keep working.
- **Authorization in services.** Handlers authenticate and delegate; they never re-implement permission logic.
- **Explicit response types.** Never serialize database rows or credential fields. Password, token, and secret material never appears in a response.
- **Consistent errors.** A single error envelope with a machine-readable code and a human-readable message; never leak internal details or the existence of private resources.
- **Rate limits and resource limits** per authenticated identity, with separate limits for unauthenticated requests and for expensive endpoints such as search, diffs, and archives.
- **Webhooks and apps** are first-class: events are versioned, payloads are signed, and GitHub App installation tokens are scoped to the installation's permissions.
- **GraphQL** exposes the same authorization decisions as REST; a field resolver must not bypass a service check.

## 12. Security model

**Authentication:** browser sessions with opaque tokens stored hashed and sent in `Secure`, `HttpOnly`, `SameSite` cookies plus a CSRF token for state changes; personal access tokens with scopes and expiry for API and Git clients; SSH keys for Git over SSH; OAuth and GitHub App flows for integrations; two-factor authentication with TOTP, WebAuthn security keys, and recovery codes; SSO via SAML and provisioning via SCIM at the organization level.

**Authorization:** one shared service decides repository access, organization roles, team inheritance, and application permissions. Browser, REST, GraphQL, HTTPS Git, SSH Git, package registries, and runners all call it. A transport that answers an access question itself is a defect.

**Untrusted input surfaces:** Git operations and repository content, rendered Markdown and diffs, archive generation, uploaded files, package uploads, webhook payloads, page builds, workflow definitions, and workspace images. Each is treated as hostile: raw HTML is disabled in rendered Markdown, archive and diff work is bounded, and no repository-provided hook is ever executed.

**Operational security:** secure password hashing (Argon2id with recorded parameters), constant-time comparison for token material, generic authentication errors, authentication rate limits, request-size limits, and audit events for consequential changes. Secrets and instance configuration stay outside the repository and out of logs.

## 13. Background work

A PostgreSQL-backed jobs table is the initial durable queue. Workers claim jobs safely, retry transient failures with bounded backoff, and record terminal failures for operator visibility. Jobs must be idempotent, because a worker can stop after doing work but before recording completion.

Examples: email delivery, notification fan-out, search indexing, pull-request diff refresh, webhook retry, package retention, workspace idle reaping, and cleanup. Authorization and ref-update correctness are never delegated to an asynchronous job; they are enforced in the request path.

A message broker may be introduced in phase 10 if measured throughput requires it, but the PostgreSQL queue remains the correctness baseline.

## 14. Observability and operations

- Structured logs with request, job, run, and workspace correlation identifiers, and no secret or repository content.
- Metrics for database health, request latency and error rates, Git operation outcomes, job and webhook failures, runner and workspace lifecycle, queue depth, and storage growth.
- Liveness and readiness endpoints that distinguish "process alive" from "dependencies usable".
- Migration commands with locking, an operator-visible way to inspect failed jobs and deliveries, and documented rollback limits per release.
- Backup and restore drills for the coordinated database, repository storage, and object storage set.

## 15. Initial domain model

- `User` owns credentials, sessions, personal access tokens, SSH keys, GPG keys, and notification settings.
- `Organization` and `Enterprise` contain users through memberships and group access through `Team`; teams may nest.
- `Repository` belongs to a user or organization and has visibility, a canonical storage ID, permission policy, protection rules, topics, and stars.
- `Issue` belongs to a repository; `PullRequest` extends the collaboration model and tracks source and target refs, review state, and merge state.
- `Comment`, `Label`, `Milestone`, `Review`, and `ReviewThread` attach to issues or pull requests; `Project` and `Discussion` provide wider planning and conversation surfaces.
- `Workflow`, `WorkflowRun`, `Job`, `Step`, `Artifact`, `Cache`, and `Secret` model CI.
- `Package` and `PackageVersion` model registries; `Release` and `ReleaseAsset` model releases.
- `Webhook`, `WebhookDelivery`, `OAuthApp`, `GitHubApp`, and `Installation` model integrations.
- `Workspace`, `WorkspaceImage`, and `WorkspaceVolume` model cloud development.
- `Notification` and `AuditEvent` reference an actor and the relevant domain object.
- `Job` (queue) stores a typed payload, status, attempt count, scheduling time, and error metadata.

Relations use stable internal IDs. Names and slugs are user-facing identifiers and may change.

## 16. Delivery sequence

The phase order, workstreams, and exit criteria are in [implementation-plan.md](implementation-plan.md), with per-area detail under `docs/roadmap/`.

| Phase | Theme |
| --- | --- |
| 0 | Service foundation (implemented) |
| 1 | Identity, accounts, repository hosting, Git over HTTPS, backup |
| 2 | Issues, organizations, teams, notifications, SSH |
| 3 | Pull requests, review, merge |
| 4 | REST, GraphQL, tokens, OAuth and GitHub Apps, webhooks |
| 5 | Actions |
| 6 | Packages and releases |
| 7 | Search, topics, wiki, pages, discussions, projects, insights |
| 8 | Branch protection, rulesets, 2FA, SSO, SCIM, audit, moderation |
| 9 | Cloud development workspaces |
| 10 | Multi-node, object storage, observability, upgrades |

Phases are ordered by dependency, not by appeal. Phase 5 and phase 9 both depend on the authorization, storage, and API contracts that phases 1–4 establish.

## 17. Decisions to revisit

Revisit this architecture when any of these becomes a concrete requirement:

- Multiple application nodes need concurrent write access to repository data.
- Repository or object storage exceeds the operational limits of the local filesystem.
- Queue throughput, search latency, or diff computation becomes a measured bottleneck.
- A single container runtime cannot isolate untrusted jobs or workspaces adequately.
- The product needs a separate frontend deployment, public cloud tenancy, or high-availability failover.

At that point, preserve the domain APIs and storage interfaces and extract only the boundary that requires independent scaling.

## 18. Long-term design constraints

- Preserve normal Git repository formats and provide export paths for metadata and repository data.
- Keep stable internal IDs separate from names, URLs, and filesystem paths.
- Keep authorization in shared application services so new transports and integrations cannot bypass it.
- Version public APIs and event payloads before external users depend on them.
- Make background work durable and idempotent before adding work that cannot finish in a request.
- Treat optional infrastructure as optional: a small self-hosted install must not need a broker, a search cluster, or a container orchestrator.
- Document upgrade, restore, and downgrade limits for every schema or storage change.
- Keep untrusted execution isolated by construction, not by configuration that can be turned off.
