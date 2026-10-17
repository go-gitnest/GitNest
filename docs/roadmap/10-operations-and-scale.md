# Operations and scale roadmap
**Phase:** 10
**Status:** not started
**Roadmap file:** docs/roadmap/10-operations-and-scale.md

## Purpose

The baseline deployment stays a single node: one `cmd/gitnest` binary plus PostgreSQL plus repository storage on local disk. This phase makes that baseline operable at size, and adds heavier components only where a workstream genuinely needs them, always with a documented fallback to the single-node baseline. It also defines the constraints that multi-node operation imposes on storage, locking, migrations, and backups, so capacity work does not quietly break correctness.

## Scope

Multi-node web deployment with shared PostgreSQL and repository storage (single-writer constraints and locking), object storage for LFS, uploads, artifacts, and package blobs, an optional search service with an SQL fallback, a durable queue and worker-pool evolution, caching layers, read replicas, observability (structured logs, metrics for database health, Git operation outcomes, job and runner failures, workspace lifecycle), performance budgets and load testing, upgrade and migration tooling with locking and rollback limits, backup and restore drills for the coordinated database plus repository plus object storage set, disaster recovery, capacity planning, and zero-downtime release strategy.

Out of scope: Kubernetes-specific packaging, managed cloud offerings, a mandatory broker or search cluster, cross-region active-active operation, and any change to the product-level exclusions recorded in `docs/product-goals.md`.

## Workstreams

### W10.1 Multi-node web deployment and single-writer constraints

- **Deliverable:** A documented and enforced deployment model for N web nodes behind a load balancer sharing one PostgreSQL primary and one repository storage path. Every operation that must not run concurrently on two nodes takes a PostgreSQL advisory lock or a row lease keyed by a stable identifier: repository creation and deletion, rename, ref updates, pack maintenance, cache and artifact pruning, and migration. Sessions remain server-side, so no sticky routing is required. The framework's current dependency on a single writable Git directory is documented as a per-repository single-writer rule rather than a per-instance rule.
- **Depends on:** `internal/platform/postgres`, `internal/platform/storage`, `internal/repository`, `internal/gittransport`.
- **Acceptance:** Two nodes started against one database and one storage path: creating the same repository concurrently through both yields exactly one repository and no orphan directory; a ref update attempt from the second node while the first holds the repository lock either waits or fails with a distinct busy error, never interleaves. A restart of one node does not drop sessions held on the other.

### W10.2 Repository storage on shared or object-backed storage

- **Deliverable:** A storage abstraction for bare repositories that supports a local path (baseline) and an object-backed mode, with the local path remaining the default. The abstraction covers create, delete, existence check, size accounting, and the reference of a repository to its location. The design keeps a repository's writable working form on a node's local disk and treats the object store as durable backing, with an explicit, documented reconciliation procedure for the copy step so a crash between the two does not silently lose a push.
- **Depends on:** W10.1, `internal/platform/storage`.
- **Acceptance:** With local storage configured, behaviour matches the pre-phase baseline. With object backing configured, a push on one node is cloneable from another node after the documented reconciliation step. A crash injected between the local write and the object copy is detected by a consistency check and reported to the operator instead of being treated as success.

### W10.3 Object storage for LFS, uploads, artifacts, and package blobs

- **Deliverable:** One `BlobStore` interface (put, get, stat, delete, list-by-prefix, presigned read where available) with a local-filesystem implementation for the baseline and an S3-compatible implementation. Actions artifacts, caches, workspace snapshots, release assets, uploaded attachments, LFS objects, and package blobs all use it, so a single configuration switch moves them together. Size accounting and per-tenant quota checks happen before a write is committed.
- **Depends on:** Actions and workspace blob usage from earlier phases; `internal/platform/storage`.
- **Acceptance:** The local implementation and the S3-compatible implementation pass the same contract test suite, including missing objects, size mismatch, and partial-write cleanup. Switching implementations requires no schema change. Every consumer works with local storage configured, so object storage is never a hard requirement for a single-node install.

### W10.4 Search service with SQL fallback

- **Deliverable:** A search adapter behind an interface with two implementations: a PostgreSQL implementation using indexed full-text search and `pg_trgm` for fuzzy matching, and an optional external search service for large instances. The external implementation is a projection with a rebuildable index, a reindex command, and a documented consistency lag. Degraded or unavailable external search falls back to SQL rather than failing the query.
- **Depends on:** `internal/search`, `internal/platform/search`.
- **Acceptance:** The SQL implementation answers every query shape used by the UI and API, including issue and pull-request search scoped by repository and by access. With the external service enabled, a reindex from an empty index reproduces the same result set for a fixture corpus. Stopping the external service makes search fall back within a bounded time, and results respect access checks in both modes.

### W10.5 Durable queue and worker-pool evolution

- **Deliverable:** The job queue grows from a single in-process worker to a pool of worker processes over the same PostgreSQL-backed queue, with priority classes, per-class concurrency limits, retry with capped backoff, dead-letter handling after a maximum attempt count, idempotency keys for jobs that must not double-apply, and a visibility timeout so a crashed worker's job returns to the queue. The existing interrupted-work recovery from earlier phases keeps working.
- **Depends on:** `internal/jobs`; Actions and workspace schedules that consume it.
- **Acceptance:** Tests cover a worker killed mid-job with the job retried exactly once after the visibility timeout, an exhausted-retry job landing in the dead-letter state with its error retained, and a duplicate enqueue with the same idempotency key producing one execution. Queue depth and oldest-job age are exposed as metrics.

### W10.6 Caching layers

- **Deliverable:** An in-process cache with explicit invalidation for permission decisions, repository metadata, and rendered Markdown, plus a shared cache layer for expensive derived data (diff summaries, tree listings, counts) when more than one node is deployed. Every cached entry has a stated invalidation trigger, an entry-count and byte bound, and a metric for hits, misses, and evictions. Permission decisions cache only within a single request or a bounded short window tied to an explicit revocation signal. The shared layer is optional: with it disabled the instance is correct and slower, never incorrect.
- **Depends on:** W10.1; `internal/platform/cache`, `internal/repository`.
- **Acceptance:** Revoking a collaborator takes effect for the next request, not after a cache window, in both single-node and multi-node tests. Cache size stays within its bound under a workload that produces more distinct keys than the bound. Disabling the shared layer entirely still produces correct results with a documented latency cost. A cache entry is keyed by a value that already accounts for the viewer's permission, so two users never share a rendered payload that one of them may not see.

### W10.7 Read replicas

- **Deliverable:** Optional read-replica support for read-only query paths, with an explicit classification of which queries may use a replica and which must use the primary (authorization decisions, session lookups, and anything read immediately after a write in the same request). Replication lag is measured and exposed; a replica that is unresponsive, lagging past a threshold, or absent is bypassed in favour of the primary. Routing is a property of the query call site, declared once, not a connection pool the caller chooses freely.
- **Depends on:** W10.1, `internal/platform/postgres`.
- **Acceptance:** Tests prove that authorization and session checks never read from a replica, that a query after a write in the same request reads the primary, and that a lagging replica is bypassed. With no replica configured, query routing is identical to the pre-phase behaviour. A replica returning stale data for a classified read-only path is covered by a test that asserts the declared classification rather than observed timing.

### W10.8 Observability: logs, metrics, and traces

- **Deliverable:** Structured request and operation logs with a stable event name, actor kind, and correlation ID; a metrics surface covering database health (connection counts, query latency, lock waits, replication lag), Git operation outcomes (clone, fetch, push, receive rejection, process timeout), job and runner failures (queue depth, lease expiry, retry, dead-letter), workspace lifecycle (create, start, stop, reap, failure, quota rejection), and HTTP latency by route class. Optional trace propagation across HTTP, queue, and Git operations. A documented log retention and redaction policy. Metric labels are bounded and never include user names, repository names, tokens, or file paths. An operator can answer "is it slow, and where" and "what failed in the last hour" from these signals alone, without reading instance data.
- **Depends on:** `internal/platform/logging`, W10.1.
- **Acceptance:** Each listed signal is emitted by the corresponding code path and covered by a test that asserts the event name or metric exists with the expected labels. A redaction test asserts that no secret, session token, access token, or repository file content reaches a log line or a metric label. Cardinality is bounded under a fixture workload with many distinct repositories. A failing Git operation, a leased-then-abandoned job, and a quota-rejected workspace each produce a distinguishable signal.

### W10.9 Performance budgets and load testing

- **Deliverable:** Published budgets for the operations that define perceived speed: `git clone` and `git fetch` throughput and time to first byte, `git push` completion, repository page render, pull-request diff render, Actions run scheduling latency, workspace start latency, and API p95/p99 latency per route class. A repeatable load-test scenario set with a seed corpus covering the workload taxonomy that matters (clone-heavy, push-heavy, browse-heavy, many small repositories versus few large ones, many concurrent workflow jobs, many concurrent workspaces), and a recorded baseline result per budget. Budget regressions are reported as test output, not as a release gate that can be silently skipped.
- **Depends on:** W10.8.
- **Acceptance:** The load suite runs against a documented instance size and produces a report with the measured value against each budget. A deliberately introduced regression in one measured path (a test double) fails the comparison. The suite documents the corpus size and the hardware class it was measured on, so results are comparable over time. Each scenario states which instance limit it is expected to reach first.

### W10.10 Upgrade and migration tooling

- **Deliverable:** A documented upgrade path with: a schema-version compatibility rule (each release states the oldest database version it can serve and the newest it can be upgraded from), migration locking so two nodes cannot migrate concurrently, a required pre-upgrade backup step, a preflight check that reports what will change, and explicit rollback limits (which migrations are reversible, which are not, and what to do instead). `gitnest migrate` gains a dry-run mode that reports pending migrations without applying them, and a status mode that reports the applied and expected versions. Migrations remain forward-only files in `migrations/`; an applied migration is never edited.
- **Depends on:** `internal/platform/postgres`, `cmd/gitnest`.
- **Acceptance:** Two concurrent `migrate` invocations against one database apply each migration exactly once, with the second either waiting or exiting clearly. The preflight output lists the migration files that would be applied and exits non-zero when the database is newer than the binary. A documented irreversible migration is flagged as such before it runs, and the upgrade document states the recovery step for each case.

### W10.11 Backup, restore, and coordination across the data set

- **Deliverable:** One documented recovery set that treats the PostgreSQL database, repository storage, and object storage as a coordinated unit, built from the earlier backup work: a quiesced or lock-coordinated backup procedure, a manifest recording what was captured and at which point, verification that the captured set is internally consistent, and a restore procedure into a clean instance. Automated or manual drills with a recorded duration, operator steps, and a pass/fail result per drill. Restore verification includes an actual clone of a restored repository and an integrity check of the Git objects.
- **Depends on:** `internal/repository`, W10.2, W10.3.
- **Acceptance:** A maintainer who did not create the backup can restore the instance by following the document alone and clone a restored repository. The verification step fails loudly when the object store is missing blobs the database references, or when a repository directory referenced by a row is absent. A drill record names the version, the date, the duration, and any step that had to be improvised (improvisation is treated as a documentation defect).

### W10.12 Disaster recovery

- **Deliverable:** Documented recovery objectives (target recovery time and target recovery point) for the single-node baseline and for a multi-node deployment, plus runbooks for the scenarios that actually occur: primary database loss, repository storage loss or corruption, object storage loss, node loss in a multi-node deployment, and a bad release. The runbooks state what is lost when the recovery point is not achieved, and which Git operations are safe to repeat after a restore (pushes are idempotent at the object level; a partially applied merge is not).
- **Depends on:** W10.11, W10.1.
- **Acceptance:** Each runbook is exercised at least once and its observed recovery time and recovery point are recorded next to its objective. A runbook that cannot be completed without information that is not in the document is a failed drill. The single-node and multi-node objectives are stated separately rather than as one aspirational number.

### W10.13 Capacity planning

- **Deliverable:** A sizing guide mapping the number of users, repositories, pushes per day, repository size distribution, Actions job minutes, and workspace concurrency to CPU, memory, database storage, repository storage, object storage, and IOPS. A `gitnest doctor`-style command (or an extension of an existing one) that reports current usage against configured limits and highlights the resource closest to its limit. Documented triggers that say when to move to multi-node, object storage, the shared cache, or the external search service.
- **Depends on:** W10.8, W10.9.
- **Acceptance:** The doctor command reports database size, repository storage size, object storage size, queue depth, and count of running jobs and workspaces on a populated fixture instance. The sizing guide's numbers are reconciled with a measured run of the W10.9 load suite, and each scaling trigger states the measured threshold that justifies it rather than a guess.

### W10.14 Zero-downtime release strategy

- **Deliverable:** A release procedure that keeps the service available across a version change: schema-first migration ordering (additive migration, deploy, then a later release removes the old shape), a compatibility rule that the previous and next release can run concurrently against one schema, a documented maximum overlap window, health-gated rollout and drain using `/api/ready` and connection draining, and a stated rollback boundary (which releases can be rolled back, and that a release which applied an irreversible migration cannot). Release artifacts remain a single binary plus built web assets served from the same origin.
- **Depends on:** W10.10, W10.1.
- **Acceptance:** A rehearsal upgrades a two-node deployment one node at a time while a client loop performs clones, pushes, and page reads, with zero failed requests and no dropped sessions. A rollout is aborted by a failing readiness gate and the old version continues serving. The release document states, per release, whether rollback is supported and which migration is the point of no return.

## Data and interfaces

- Schema additions (`migrations/NNN_operations_*.up.sql`, appended forward, never editing an applied migration): `job_queue` extensions for priority, visibility timeout, retry count, idempotency key, and dead-letter state; `instance_settings` for limits reported by the doctor command; `storage_backends` if the mapping from a repository to a backend must be recorded; `migration_history` already exists as the migration ledger and gains no destructive change.
- Packages: `internal/platform/postgres` (replica routing, lock helpers, migration tooling), `internal/platform/storage` (repository storage modes plus the shared `BlobStore`), `internal/platform/cache`, `internal/platform/search`, `internal/platform/logging` (structured events and metrics), `internal/jobs` (queue extensions, worker pool), `internal/search`, `internal/repository`, `internal/gittransport`, `cmd/gitnest` (migrate dry-run and status, doctor).
- Operational interface: `/api/health` (liveness) and `/api/ready` (database and dependency readiness) keep their current contract; metrics are served on a separate documented listener that can be bound to a private address.
- Configuration: database primary and optional replica URLs, repository storage mode and path, object storage endpoint/bucket/credentials, external search endpoint, cache size bounds, worker pool sizes and priorities, log level and retention, metrics listener address.
- Documentation deliverables: an upgraded `README.md` operations section, `docs/operations/backup-restore.md`, `docs/operations/disaster-recovery.md`, `docs/operations/upgrade.md`, `docs/operations/sizing.md`, and `docs/operations/observability.md`.
- Migration discipline: every schema change in this phase is a new file in `migrations/`. No migration in this phase removes a column or table that a supported previous release still reads; removal is a separate release after the compatibility window stated in the zero-downtime procedure. No migration acquires an exclusive lock on a large table for longer than the documented budget, and a migration that must rewrite data states its expected duration.

## Security notes

- Multi-node operation must not weaken authorization: replicas never serve authorization or session reads, caches invalidate on an explicit revocation signal, and locks protect every operation that must be exclusive.
- Object storage credentials are read from configuration or a file outside the repository tree, are never logged, and are scoped to the single bucket the instance uses. Presigned URLs, where used, are short-lived and bound to one object.
- Metrics labels and log fields are bounded and carry no user names, repository names, branch names, tokens, secrets, file contents, or client IP addresses beyond what the audit log already records under its own retention policy.
- The metrics and doctor surfaces expose instance-wide operational data; they require administrator authentication, or a listener bound to a private address, and never expose cross-tenant content.
- Backup artifacts contain the full database and all repository content and are treated as the most sensitive data the instance holds: encrypted at rest, access-controlled, and never written to a shared temporary directory.
- Queue payloads and dead-letter records must not contain secrets, tokens, or file contents; a job that needs a secret fetches it at execution time under its own authorization.
- Migration and lock helpers must not be reachable from an HTTP transport; they run from `cmd/gitnest` only.
- Load-testing and doctor commands must be safe against a production database: bounded concurrency, read-only where possible, and an explicit confirmation requirement before any command that writes.
- Every document in this phase is written for the operator who is on call, not for the reader evaluating the project: it names the command, the expected output, and the point at which the operator should stop and escalate.

## Out of scope

Kubernetes operators and Helm charts; managed database or object-storage offerings; cross-region active-active replication with conflict resolution; a mandatory message broker or search cluster; multi-tenant hard isolation within one instance; and any change to the product-level exclusions in `docs/product-goals.md`. The mobile app, GitHub's Codespaces, Advanced Security, Gist, payments, and AI features remain out of scope.

## Exit criteria

The single-node baseline still installs and runs with one binary, PostgreSQL, and local disk, with no mandatory new dependency. A two-node deployment shares one database and one repository storage path, with every exclusive operation protected by a lock or lease and verified by concurrent tests. Object storage, the shared cache, and the external search service are each optional, contract-tested, and switchable without a schema change, and the system falls back without them. Metrics cover database health, Git operation outcomes, job and runner failures, and workspace lifecycle, with redaction proven by test. Upgrade, backup, restore, disaster recovery, and zero-downtime release procedures are documented and each exercised at least once, with recorded durations and observed recovery points. Published performance budgets have a recorded baseline and a repeatable load suite. `gofmt -l cmd internal migrations` is empty, and `go vet ./...`, `go build ./...`, and `go test ./...` pass, with `go test -race ./...` passing when `GITNEST_TEST_DATABASE_URL` is set.

## Operational checkpoints

Four routines turn the workstreams above into something a maintainer actually does. Each names the workstream that supplies its inputs, so a routine cannot be claimed before its data exists.

- **Before every release:** run the upgrade preflight from W10.10 against a copy of the production database, confirm the backup from W10.11 completed and verifies, and check the W10.13 doctor output against the sizing thresholds.
- **Weekly:** read the W10.8 signals for queue depth, oldest-job age, replication lag, and failed Git operations; investigate any signal that moved without a corresponding change in load.
- **Monthly:** run the W10.9 load suite against a staging instance and compare each budget to the recorded baseline; a budget that drifted is either accepted explicitly in writing or treated as a regression.
- **Quarterly:** run one W10.11 restore drill and one W10.12 scenario runbook end to end, and update the recorded recovery time and recovery point with the observed values.

## Scaling triggers

Each heavier component is introduced by a measured condition, not by preference. Until the condition holds, the single-node baseline stays the supported configuration.

| Component | Move to it when | Fallback that must keep working |
| --- | --- | --- |
| Second web node | Readiness or throughput on one node is the measured limit, or a single node cannot meet the availability target | One node, same database, same storage |
| Shared repository storage or object backing | Repository storage outgrows one host's disk, or more than one node must serve clones | Local repository root on one node |
| Object storage | Blob volume (LFS, artifacts, caches, snapshots, packages) outgrows local disk or must outlive a node | Local filesystem `BlobStore` |
| Shared cache | Node-local caches diverge enough that derived data is recomputed on every node | In-process cache only |
| Read replica | Read load contends with writes on the primary, measured as query latency or lock waits | All queries on the primary |
| External search service | SQL full-text search exceeds its latency budget on the instance's corpus | PostgreSQL full-text search and `pg_trgm` |
| Worker process pool | Queue depth or oldest-job age grows while the web process is healthy | In-process worker over the same queue |

The triggers are recorded with their measured thresholds in the sizing document, and a trigger that fires without its threshold being met is a documentation defect rather than a reason to add a component.
