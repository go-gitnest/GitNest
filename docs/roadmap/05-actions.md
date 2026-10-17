# Actions roadmap
**Phase:** 5
**Status:** not started
**Roadmap file:** docs/roadmap/05-actions.md

## Purpose
Give GitNest a CI/CD system that reads GitHub-compatible workflow files, runs untrusted user code in an isolated job executor outside the web process, and reports check results back to pull requests. The workflow authoring model, trigger names, context variables, and token semantics must match GitHub closely enough that an existing `.github/workflows` directory works without edits. Nothing in this phase runs inside `cmd/gitnest` or the HTTP handlers; job execution is a separate process with no database credentials.

## Scope
Workflow discovery, schema, and validation; trigger model; job graph; expression and context language; secret masking; matrix expansion; step execution; JavaScript, Docker, and composite actions; reusable workflows; job and service containers; caching; artifacts and retention; logs with live streaming; concurrency groups; environments with protection rules; secrets and variables at repository, organization, and environment scope; self-hosted runners with labels and registration tokens; OIDC token issuance; publishing run and job status for pull-request checks; the workflow token permission model; rerun and cancel; quotas and abuse limits; the runner protocol and scheduler.

Excluded by product scope: Advanced Security (code scanning, secret scanning, Dependabot), Marketplace distribution and payments, Copilot and AI features, Sponsors. The GitHub Apps framework is in scope but owned by the integrations roadmap, not this one.

## Workstreams

### W5.1 Workflow file discovery and schema validation
- **Deliverable:** `internal/actions/workflow` parses `on`, `jobs`, `steps`, `permissions`, `concurrency`, `env`, `defaults`, and `strategy` with a strict YAML decoder that rejects duplicate keys, unknown top-level keys, unknown step types, and unsupported syntax. Publish a JSON Schema under `docs/` for editor use. Cap workflow file size and job/step counts at parse time. Discovery order and file naming follow GitHub: `.github/workflows/*.yml` and `*.yaml`, evaluated at the triggering ref rather than at the default branch.
- **Depends on:** `internal/repository` read access for the workflow path at a given ref.
- **Acceptance:** Tests cover malformed YAML, duplicate keys, oversized files, unknown keys, and a valid multi-job workflow. A workflow definition is stored with the exact commit SHA it was read from, and a run's definition is immutable after creation. A workflow added on a feature branch runs for that branch's push without being merged first, and a disabled workflow produces no run.

### W5.2 Trigger model and event ingestion
- **Deliverable:** `push`, `pull_request` (with `types`, `branches`, `paths` filters), `schedule` (UTC cron, minimum interval enforced), `workflow_dispatch` with typed inputs, `repository_dispatch` with a bounded JSON payload, `release`, and tag triggers. Events are persisted as durable run requests before any run is created, so a process restart does not lose a triggered workflow. Dispatch payload size, schedule count per repository, and input count are all bounded.
- **Depends on:** W5.1; the durable job record introduced for notifications; `internal/repository` ref and tag changes.
- **Acceptance:** One `push` produces exactly one run per matching workflow even if the process restarts between event receipt and run creation. Non-matching branch and path filters produce no run. `workflow_dispatch` rejects inputs that do not match the declared types and rejects an unknown input name. A `pull_request` from a fork creates a run with no repository secrets available. A duplicate delivery of the same event does not create a second run.

### W5.3 Job graph, `needs`, and the scheduler
- **Deliverable:** `internal/actions/scheduler` builds a DAG from `needs`, rejects cycles at parse time, skips dependents of a failed job, and runs independent jobs concurrently under a configurable instance limit. The scheduler claims runnable jobs from the durable queue with a lease and a heartbeat. A job whose `needs` were skipped is itself skipped rather than run.
- **Depends on:** W5.2; `internal/jobs` durable queue semantics.
- **Acceptance:** Tests cover diamond graphs, fan-out, a cycle rejection, and failure propagation with `if: always()`. A job never starts before every `needs` job reaches a terminal state. A worker that dies mid-job has its lease expire and the job is retried or failed deterministically, not double-run. Instance concurrency is never exceeded by concurrent scheduling attempts.

### W5.4 Expression and context language with masking
- **Deliverable:** `internal/actions/expr` implements the workflow expression language: `github`, `env`, `vars`, `secrets`, `inputs`, `needs`, `job`, `steps`, `runner`, `matrix`, `strategy` contexts; operators `==`, `!=`, `<`, `<=`, `>`, `>=`, `&&`, `||`, `!`; functions `contains`, `startsWith`, `endsWith`, `format`, `join`, `toJSON`, `fromJSON`, `hashFiles`; status functions `success()`, `failure()`, `cancelled()`, `always()`. Evaluator has no filesystem, network, or process access beyond `hashFiles` inside the checked-out workspace, and `hashFiles` is bounded in matched files and total bytes. A `Masker` registers every resolved secret value, plus its base64 form, and rewrites output before it is stored or streamed.
- **Depends on:** W5.1.
- **Acceptance:** Table-driven tests cover type coercion, truthiness rules, and each function. A step that prints a secret in plain and base64 form produces a log with both redacted. An expression cannot read a secret that was not passed to that job, and reading an undefined context property yields an empty value rather than an error only where the language says so. Malformed expressions fail the job with a parse error, not a runtime panic.

### W5.5 Matrix expansion
- **Deliverable:** Full `matrix` with `include` and `exclude` matching GitHub semantics, expansion at run creation rather than job execution, and an explicit cap on generated jobs per workflow. Exceeding the cap fails the run before any job is scheduled. A matrix value may not be sourced from a secret, and the expansion is recorded so the UI and API can show the planned job set before it runs.
- **Depends on:** W5.3, W5.4.
- **Acceptance:** `include`/`exclude` fixtures match expected job sets. A matrix just under the cap expands; one over the cap fails the run with a clear message and schedules nothing. Matrix values are validated to contain no expression-supplied secret material. Two jobs from the same matrix run concurrently under the instance limit rather than serializing.

### W5.6 Step execution, shells, and timeouts
- **Deliverable:** `run` steps execute inside the job's isolated context with explicit working directory, environment precedence (`env` at workflow/job/step level, plus runtime-provided values), per-step and per-job timeouts, exit-code handling, `continue-on-error`, `if` conditions, and output capture from `GITHUB_OUTPUT`. Bash, `sh`, `pwsh`, and `python` shells are supported; an unsupported shell fails at parse time. Environment values a workflow may set are listed explicitly; the caller's own environment is not inherited.
- **Depends on:** W5.3, the job isolation boundary in W5.15.
- **Acceptance:** Tests cover environment precedence, timeout termination with a distinct conclusion, `continue-on-error`, and step output propagation to a later step. A timed-out step's process tree is terminated and reaped; no orphan process survives the job. A step whose command is not found fails with a distinct conclusion from a non-zero exit. A failing `if` expression fails the step rather than being treated as false.

### W5.7 JavaScript and Docker action execution
- **Deliverable:** `internal/actions/resolver` resolves `uses` from a local path or a remote reference pinned by full commit SHA, caches the fetched action under a size-capped directory, and rejects unpinned refs. Executes Node actions in the bundled runtime and Docker actions through the container adapter, providing `INPUT_*` variables and the `GITHUB_OUTPUT`, `GITHUB_ENV`, `GITHUB_PATH`, and `GITHUB_STEP_SUMMARY` files. Actions never receive database credentials, host paths, or the GitNest signing key. Every `uses` reference a run resolved is recorded on the run with its resolved SHA and source.
- **Depends on:** W5.6, `internal/platform/container`.
- **Acceptance:** Tests cover a local action, a SHA-pinned remote action from cache, an unpinned ref rejection, and a fetch failure. `INPUT_*` casing rules and output-file parsing have fixtures. A remote action cannot write outside the action cache or the job workspace. A run record identifies the exact action commits it executed, so a later investigation does not depend on a moving branch.

### W5.8 Composite and reusable workflows
- **Deliverable:** Composite actions with nested `steps`, plus reusable workflows invoked with `uses: ./.github/workflows/x.yml` and `workflow_call` inputs, outputs, and `secrets: inherit`. Nesting depth is bounded and recursion is rejected. Secret inheritance is explicit and recorded on the run. Inputs are validated against the called workflow's declarations, and `secrets: inherit` is resolved against the caller's available secrets at call time rather than at parse time.
- **Depends on:** W5.7, W5.14.
- **Acceptance:** Tests cover three-level nesting, a recursion rejection, declared-required inputs not supplied (fails), and `secrets: inherit` from a fork pull request (inherits nothing). Output mapping from a reused workflow into the caller's `needs` context has a fixture. An undeclared input is rejected rather than passed silently, and a secret the caller cannot read is not inherited even with `inherit` set.

### W5.9 Job containers and service containers
- **Deliverable:** Job containers (`jobs.<id>.container`) and service containers (`services`) with image, environment, ports, volumes limited to run-scoped paths, credentials, and health checks. Services are reachable by service name inside the job network and by no other name. Container teardown has a documented order with a grace period, then a forced stop, and per-service resource caps that sum against the job's limit rather than each getting the full instance allowance. Health-check failure fails the job with service logs attached, bounded in size.
- **Depends on:** `internal/platform/container`, W5.6.
- **Acceptance:** A job reaches a service by name and connects to its declared port. A service that never becomes healthy fails the job within the health-check timeout. On completion or timeout, every container and network created for the job is removed; a test asserts no leftover containers after a forced stop and after a worker crash. When no container runtime is configured, a container job fails with an explicit message and never falls back to running on the host.

### W5.10 Caching, artifacts, and retention
- **Deliverable:** One blob-backed store for caches and artifacts built on a `BlobStore` interface in `internal/platform/storage` (local disk on the baseline, pluggable later). Caches use exact keys plus prefix `restore-keys`, branch-scoped visibility, per-repository and instance size caps, and LRU eviction. Artifacts use a name, per-artifact retention days, and a capped job-summary Markdown. A scheduled prune job deletes expired artifacts and evicted cache blobs. Runners authenticate cache and artifact requests with a run-scoped, short-lived token. Interrupted uploads are discarded rather than left half-visible.
- **Depends on:** W5.7.
- **Acceptance:** Tests cover exact cache hit, prefix restore, miss, branch-scoping isolation (a cache written on a protected branch is not readable from a fork PR), eviction at the cap, and rejection of cross-repository reads. Retention pruning is covered with an injected clock. An anonymous caller cannot download a private repository's artifact, and oversized, empty, or duplicate-name uploads fail predictably. Deleting a run deletes its artifacts and blobs. A cache save that would exceed the repository cap is refused with a clear reason instead of evicting unrelated entries silently.

### W5.11 Logs with live streaming and step search
- **Deliverable:** Logs are written per step as bounded chunks, masked before storage, with recorded step boundaries. Endpoints serve completed logs (with range reads), a live stream for a running job, and search over a job's log with step filtering. A per-job log size cap truncates with an explicit marker instead of growing unbounded. A retention window deletes logs with their run, and no log read path can reach across jobs.
- **Depends on:** W5.4 masking, W5.7.
- **Acceptance:** Masking is verified against stored logs, not only the stream. A slow or disconnecting client does not grow server memory beyond the per-job buffer cap. Step boundaries are returned by search. Truncation is visible in the log body and the API response. A user with repository read access can read a log; a user without it gets the same not-found response as for a private repository, never a partial log.

### W5.12 Concurrency groups, cancel, and rerun
- **Deliverable:** `concurrency` group with `cancel-in-progress`, implemented as a durable lease keyed by repository and group name so at most one run per group runs at a time and at most one is pending. Cancel terminates the run's processes and marks non-started jobs cancelled. Rerun reuses the original definition, failing jobs only, or all jobs; reruns never observe changed secrets implicitly and record that they are reruns. Cancel and rerun are themselves idempotent: a second request on a terminal run reports the terminal state rather than starting a new run.
- **Depends on:** W5.3, W5.6.
- **Acceptance:** Three simultaneous runs in one group leave exactly one running and one pending; the superseded run is cancelled when `cancel-in-progress` is set. A dead worker's lease expires and releases the group. Cancel of a running job reaps its process tree and reaches a terminal run state within a bounded time. Rerun of a completed run produces a new run ID linked to the original. A rerun request against a running run is refused.

### W5.13 Environments, protection rules, and environment secrets
- **Deliverable:** Environments per repository with required reviewers, wait timer, and branch/tag restrictions, plus environment-scoped secrets and variables. A job that names an environment creates a pending deployment that must be approved before the job starts; approvals record actor and timestamp. Environment secrets are injected only after protection passes. Environment CRUD and the pending-deployment decision live in a shared application service called by both browser and API transports. Protection-rule changes and approval decisions are audited, and a pending approval expires rather than waiting forever.
- **Depends on:** W5.14, W5.3.
- **Acceptance:** A non-required reviewer is denied approval. A wait timer blocks start until expiry. A disallowed branch is refused. A job that never passed protection has no environment secret in its environment, verified inside the executor. An expired pending deployment fails the job instead of holding the runner, and the audit trail names who changed a rule and who approved.

### W5.14 Secrets, variables, and the workflow token permission model
- **Deliverable:** Repository, organization, and environment secrets and variables, with secrets encrypted at rest under an instance key (`GITNEST_SECRET_KEY`) and never returned by any API after creation. The workflow token (`GITHUB_TOKEN` equivalent) is minted per run with a default read-only permission set, widened only by the workflow's `permissions` block, scoped to the originating repository, and expiring at run end. Fork pull requests get a read-only token and no secrets. Declared permissions are enforced by the shared authorization service, not by the Git transport or HTTP layer. Organization scope is opt-in per repository.
- **Depends on:** W5.1, `internal/repository` permission service, `internal/organization`.
- **Acceptance:** Tests prove default read-only behavior, that widening requires an explicit `permissions` entry, token expiry after run end, denial of writes the workflow did not request, and absence of all repository secrets in a fork PR run. No endpoint returns a stored secret value; write-only rotation is covered by a test. A repository that has not opted in to an organization secret cannot read it, and a revoked token fails on the next use.

### W5.15 Runner protocol, self-hosted runners, OIDC, and quotas
- **Deliverable:** A versioned JSON-over-HTTP runner protocol in `internal/runner` (register, poll, lease, heartbeat, status, log chunk, artifact and cache verbs) with contract tests, resumable log chunk upload, and lease expiry. Self-hosted runners with labels, scoped registration tokens (repository/organization/instance) that are single-use and expiring, and label matching in the scheduler. OIDC token issuance for a job with a claim set bound to repository, ref, workflow, and run, signed with a rotatable instance key, and a configurable allowed-subject audience. Per-user and per-repository quotas for concurrent jobs, total job minutes, artifact bytes, and cache bytes, with a clear rejection reason. Protocol changes are additive within a major version, and a runner older than the supported minimum is refused at registration with a distinguishable error.
- **Depends on:** W5.3, W5.6.
- **Acceptance:** Contract tests cover every protocol verb, an unknown verb, and a version mismatch. An expired or reused registration token is refused. A job with an unmatched label stays queued and never runs on a mismatched runner. An OIDC token verifies against the published JWKS and fails for a wrong audience. Quota exhaustion fails new runs with a distinguishable error and does not affect already running jobs. The scheduler degrades to the single-node baseline when no external runners are registered, and a runner that stops heartbeating has its lease released without losing the job's logs.

### W5.16 Run and check status reporting
- **Deliverable:** Run and job conclusions are published into `internal/pullrequest` and `internal/activity` as check runs keyed by head SHA and workflow/job name, with a documented rule for which result is authoritative when a name is reported twice. Reporting is a contract this phase owns; the required-check configuration itself, its merge blocking, and its evaluation against this data are owned by the governance phase (W8.3), so this workstream must not build a second merge gate. Stale results from a superseded head SHA are never counted, check output and annotations are bounded in size and rendered as untrusted text, and a run that was cancelled or skipped reports that state explicitly rather than reporting success.
- **Depends on:** W5.3, W5.12, `internal/pullrequest` check-run ingestion contract.
- **Acceptance:** Tests cover a result reported for the current head SHA, a result reported for an older head SHA (ignored or superseded, never applied to the newer commit), a re-run updating the same check instead of adding a duplicate, and a cancelled run reporting cancelled rather than passing. The published contract is consumed unchanged by W8.3, verified by a test that evaluates a required check against a reported result. Emitting check data does not require any privilege beyond repository read on the reported repository.

## Data and interfaces
- Schema additions (`migrations/NNN_actions_*.up.sql`, appended forward, never editing an applied migration): `workflows`, `workflow_run_requests`, `workflow_runs`, `workflow_jobs`, `job_steps`, `job_logs`, `action_cache_entries`, `artifacts`, `cache_entries`, `concurrency_leases`, `environments`, `deployment_approvals`, `secrets`, `variables`, `runner_registrations`, `runner_leases`, `workflow_quotas`, `check_runs`.
- Run identity: a run is identified by a UUID and stores the repository ID, the triggering event, the actor, the head SHA, the workflow path, and the exact parsed definition revision. Nothing about a run is keyed by a user-supplied path or name.
- Job lease and queue columns on `workflow_jobs`: state, runner ID, attempt, lease expiry, heartbeat time, and the matrix payload. The scheduler reads and writes these in one transaction so two workers cannot claim one job.
- Packages: `internal/actions` (workflow parse, expr, scheduler, resolver, executor-facing application services), `internal/runner` (protocol, registration, label matching, OIDC claims), `internal/jobs` (durable queue, leases), `internal/platform/container` (job, service, and action containers), `internal/platform/storage` (artifact, cache, and log blobs, alongside the existing repository store), `internal/platform/postgres`, `internal/platform/logging`, `internal/transport/http/api` (Actions REST surface), `internal/transport/http/web` (run and log views), `internal/pullrequest` (required checks), `internal/webhooks` (workflow run events for installation consumers).
- API surface: `/api/v1/repos/{owner}/{repo}/actions/workflows`, `.../workflows/{id}/dispatches`, `.../runs`, `.../runs/{id}`, `.../runs/{id}/cancel`, `.../runs/{id}/rerun`, `.../runs/{id}/logs`, `.../jobs/{id}/logs`, `.../jobs/{id}/logs/stream`, `.../artifacts`, `.../artifacts/{id}`, `.../cache`, `.../secrets`, `.../variables`, `.../environments`, `.../environments/{name}/deployments`, plus `/api/v1/runner/*` for the runner protocol.
- Authorization: every Actions endpoint calls the `internal/repository` permission service for read, write, or admin before returning data, including log, artifact, and cache routes. Cache and artifact verbs used only by runners authenticate with a run-scoped token and are not reachable with a browser session.
- Events: `workflow_run` (queued, in_progress, completed), `workflow_job` (queued, in_progress, completed), `deployment_status`, emitted through the outbox used for other notifications so delivery survives restarts.
- Configuration: instance-level limits (concurrent jobs, job minutes, artifact bytes, cache bytes, log bytes, action cache bytes), container runtime endpoint, runner enrollment policy, OIDC signing key and audience, `GITNEST_SECRET_KEY` for secret envelope encryption.
- Planned design spec: `docs/design/actions.md`, written before W5.1 lands, covering the runner protocol version, the queue schema, and the worker-to-database role.

## Security notes
- Job execution never runs in the web process and never receives database credentials. Workers connect to PostgreSQL with a role limited to run, job, log, lease, and cache tables, and never to the identity or session tables.
- Every job runs with explicit CPU, memory, disk, process-count, and wall-clock limits, plus a bounded log and artifact size. The container runtime is the primary boundary; a label that permits host execution is opt-in, documented, and off by default.
- Job containers are created fresh per job and removed on completion, failure, timeout, and cancel. A reconciler removes containers left behind by a worker crash.
- Network access from a job is controlled by an egress policy with a default-deny posture for the job network and a configurable allowlist for package and registry hosts.
- Secrets are injected at job start into the job's environment or a file removed after the job, never persisted in the run definition, the queue payload, or the database.
- Secret values pass through the masking registry before any storage or streaming path; masking is tested against base64, multi-line, and JSON-escaped values.
- The workflow token is least-privilege by default, scoped to one repository, and cannot be widened by a workflow without an explicit `permissions` entry that repository policy allows.
- Fork pull requests run with no secrets and a read-only token. Environment protection rules are evaluated by the shared service before secret injection, so no transport can bypass them.
- Self-hosted registration tokens are single-use, expiring, and scoped; a runner that presents a token for one scope cannot lease jobs from another.
- Untrusted input: YAML, expression strings, `uses` refs, artifact and cache names, job outputs, and log content. Refs must be pinned by SHA; paths are resolved from database IDs, never from request strings.
- Containers never mount the host repository root, the container runtime socket, or the GitNest configuration directory. Service and job images are pulled from configured registries with a size and digest policy.
- Logs, errors, and metrics never contain secret values, token values, or private repository content, including in the failure path of the executor itself.

## Out of scope
Advanced Security (code scanning, secret scanning, Dependabot alerts and updates); Marketplace actions distribution and payments; Copilot and AI-generated workflows; GitHub-hosted runner billing; the mobile app. Also excluded: running repository-provided hooks, granting a job write access to GitNest's own administrative surface, and reusing a GitHub-hosted runner image registry that this project does not control.

## Exit criteria
A repository with a committed `.github/workflows` directory can be pushed to and produce runs for `push`, `pull_request`, `schedule`, `workflow_dispatch`, and tag events. A multi-job workflow with a matrix, a cache, an artifact, a reusable workflow, a service container, and a reported check status completes end to end, with logs streamed live and searchable by step. Every run executes outside the web process under enforced resource limits, receives no host or database credentials, and is cleaned up deterministically on success, failure, timeout, and cancel. Reported status is accurate for the head SHA it names, and the governance phase's required-check gate can consume it without further changes here. Fork pull requests provably receive neither secrets nor a writable workflow token. Integration tests gated by `GITNEST_TEST_DATABASE_URL` plus container-runtime tests cover the allowed and denied cases for secrets, the workflow token, environment approvals, cache isolation, and runner registration.

## Dependency order

The order below is a dependency order, not a schedule. Each arrow means the later workstream cannot be reviewed on its own until the earlier one has landed.

```text
W5.1 parse -> W5.2 triggers -> W5.3 scheduler -> W5.5 matrix
                 |                  |
                 v                  v
              W5.4 expr  ->  W5.6 steps -> W5.7 actions -> W5.8 reusable
                                    |            |
                                    v            v
                              W5.15 runner   W5.10 cache/artifacts
                                    |            |
                                    v            v
                              W5.9 containers   W5.11 logs
                                    |
                                    v
                     W5.13 environments, W5.14 secrets, W5.12 concurrency
                                    |
                                    v
                              W5.16 status reporting
```

Two constraints matter more than the order itself. The isolation boundary in W5.15 must exist before any workflow executes on behalf of a real user, and it cannot be retrofitted after W5.6 starts running commands. Secret handling in W5.14 must exist before environment approvals in W5.13 mean anything, because an approval that gates a secret is worthless if the secret was already reachable elsewhere.

