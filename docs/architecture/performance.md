# Performance budgets and limits

**Status:** Target design. The repository currently contains documentation only; nothing described here is implemented. The numbers below are phase-1 decisions for the rebuilt codebase.
**Scope:** Per-surface latency budgets, resource and size limits, quotas, rate limits, database budgets, the index policy that makes those budgets reachable, and the evidence required to change a budget.
**Related:** [testing.md](testing.md) defines the tiers and the tests that enforce every row of this document.

## 1. How a budget is set and enforced

1. A budget is a number with a measurement and a test. Prose alone is not a budget, and a budget with neither a metric, a log field, nor a test is deleted or converted into one.
2. Budgets are declared before the surface is implemented. A new page, route class, registry kind, or protocol verb without a row in this document is incomplete work.
3. Every limit is enforced at the earliest boundary that can reject cheaply: a size header check before reading a body, a cap check before unpacking, a depth check before parsing, an admission check before spawning a process.
4. Hitting a limit produces an explicit error or an explicit truncation marker. Silent truncation is a defect. The marker is part of the API response and the page, not only the log.
5. Limits double as security controls. Size, time, memory, concurrency, egress, and quota caps may not be raised without the review in §15.
6. Defaults must keep the single-node baseline runnable. Every number is operator-configurable unless the row marks it fixed.
7. A regression is a failing test that prints the measured value, never a warning that can be skipped.
8. Optional heavy components (external search, object storage, a shared cache, more nodes) change headroom, not a budget. A degraded mode must either still meet the budget or report the degradation in the response.
9. A limit lives in exactly one configuration layer. A policy limit is an instance setting: it is changed in the operator console, writes an audit event, and takes effect on the next evaluation without a restart. An infrastructure limit is a startup environment variable and requires a restart. The Setting column of each table names one or the other; the split is defined in [configuration.md](configuration.md).
10. A row marked `fixed` is a code constant with no configuration path.

## 2. Reference instance class and measurement method

| Item | Value |
| --- | --- |
| Instance class R1 | 4 vCPU, 8 GiB RAM, local SSD, PostgreSQL on the same host, single node, no external search or object storage |
| Corpus C1 | 1,000 repositories (median 5 MiB, largest 1 GiB), 100,000 commits, 10,000 issues, 5,000 pull requests, 20,000 users, 5,000 package versions, 20,000 stars |
| Server-side measurement | Durations and counts from the metrics in §14, over a 10-minute steady-state window with at least 10,000 samples |
| Client-side measurement | The browser tier on a documented profile: cold cache, 4x CPU throttle, Slow 4G network |
| Warm state | Read budgets are measured with a warm process and warm database cache, after a documented warm-up. Cold start is measured separately and is not a page budget |
| Re-baselining | The instance class or the corpus changing invalidates the baselines; both are recorded in `docs/operations/sizing.md` and in the load report |

Percentiles are p95 unless a row states otherwise. A budget measured on a different class is not comparable and is labeled as such.

## 3. Per-surface budgets

The numbers in this section are engineering targets enforced by tests and metrics, not instance settings; they are not configurable by an operator.

### 3.1 Server-rendered pages

Budgets are server time (the handler through the last byte written), measured per route class, excluding Git object transfer and blob download.

| ID | Route class | p95 | p99 |
| --- | --- | --- | --- |
| PAGE-REPO | Repository overview | 250 ms | 800 ms |
| PAGE-TREE | Directory listing | 250 ms | 800 ms |
| PAGE-FILE | File view | 300 ms | 1 s |
| PAGE-COMMITS | Commit list | 250 ms | 800 ms |
| PAGE-LIST | Issue and pull-request lists | 250 ms | 800 ms |
| PAGE-ISSUE | Issue detail with timeline | 300 ms | 1 s |
| PAGE-PR | Pull-request conversation | 350 ms | 1.2 s |
| PAGE-PR-FILES | Pull-request files, first page | 500 ms | 2 s |
| PAGE-SEARCH | Search results | 400 ms | 1.5 s |
| PAGE-SETTINGS | Settings, profile, administration | 200 ms | 600 ms |
| PAGE-INSIGHTS | Insights and graphs | 600 ms | 2 s |

Additional page requirements:

1. Time to first byte is within 100 ms of the server-time budget.
2. Rendered HTML is at most 100 KiB gzip per page. A page exceeding it must move data behind an API call or pagination; a test asserts the limit for each route class.
3. Diff, log, artifact, package blob, archive, and Git pack responses are excluded from these budgets and are governed by §4, §5, and §9.
4. A page that cannot meet its budget under a documented expensive input must degrade explicitly: truncate with a marker, paginate, or move computation to a durable job.

### 3.2 API

| ID | Class | p95 | p99 | Notes |
| --- | --- | --- | --- | --- |
| API-READ | REST read | 200 ms | 800 ms | Includes list endpoints with a count query |
| API-WRITE | REST write | 500 ms | 2 s | Excludes work moved to a durable job, which returns `202` with a status resource |
| API-GQL | GraphQL query | 300 ms | 1 s | Complexity at most 200 points, depth at most 10, before execution |
| API-ERR | All | 5xx rate at most 0.1% of requests | | 4xx is not an error for this budget |

1. A response body is at most 1 MiB unless the endpoint is explicitly a paginated collection or a stream; a larger payload requires pagination or a stream, and the limit row is updated in the same change.
2. Every endpoint that exceeds its budget while still succeeding records a `budget_exceeded` log field, so the regression is visible without a load run.

### 3.3 Query count per request

| Route class | Maximum SQL statements | Measured by |
| --- | --- | --- |
| HTML page | 20 | Counting driver wrapper in the T2 tier and the `gitnest_http_db_queries` metric |
| HTML pull-request files page | 30 | Same |
| REST read | 10 | Same |
| REST list | 12, including the count query | Same |
| REST write | 15 | Same |
| GraphQL query | 25 | Same |
| Background job, per processed item | 10 | Same |

1. A query executed inside a loop is a defect regardless of the total. The counting wrapper records the distinct statement shapes, so an N+1 pattern fails the test even when the total is under the cap for a small fixture.
2. Tests run against a fixture large enough to expose N+1 behaviour (at least 30 children per parent).
3. Production records the same count per route class and alerts at twice the budget.

### 3.4 Frontend bundle and interactivity

| ID | Budget | Value |
| --- | --- | --- |
| WEB-JS-INITIAL | Initial route JavaScript, gzip | 250 KiB |
| WEB-JS-CHUNK | Any route chunk, gzip | 300 KiB |
| WEB-JS-VENDOR | Shared vendor chunk, gzip | 150 KiB |
| WEB-JS-MODULE | Largest single module, gzip | 100 KiB |
| WEB-CSS | All CSS for the initial route, gzip | 60 KiB |
| WEB-FONT | Web fonts, compressed | 100 KiB |
| WEB-TTI | Time to interactive, p75 on the reference client profile | 2.5 s |
| WEB-LCP | Largest contentful paint, p75 | 2.5 s |
| WEB-INP | Interaction to next paint, p75 | 200 ms |
| WEB-CLS | Cumulative layout shift, p75 | 0.1 |

1. `web/scripts/check-bundle-size.mjs` fails CI when a budget is exceeded by more than a 10% tolerance and prints the largest twenty modules by gzip size.
2. A route may not statically import another route's chunk. The check fails on a cross-route import.
3. Source maps are not shipped in the production bundle.
4. The browser tier measures TTI, LCP, INP, and CLS against the same budgets. A first-party measurement sent to the instance is optional, off by default, and never third-party analytics.

## 4. Git operation limits

Every Git invocation runs under a bounded context, with a controlled environment, argument arrays, a process group that is killed on cancellation, and temporary data removed on success, failure, and cancellation.

| ID | Limit | Default | Setting | Enforcement |
| --- | --- | --- | --- | --- |
| GIT-PUSH-BYTES | Pack bytes accepted per push | 2 GiB | `git.max_push_bytes` | Size check on `Content-Length` and on streamed bytes before unpacking; `413` before any object is written |
| GIT-OBJECT-BYTES | Single object size | 2 GiB | `git.max_object_bytes` | `receive.maxInputSize` and a pre-receive check; documented rejection |
| GIT-REFS | Refs changed per push | 100 | `git.max_refs_per_push` | Pre-receive check; the ref transaction is all-or-nothing |
| GIT-FETCH-TIMEOUT | Clone or fetch wall clock | 30 min | `git.fetch_timeout` | Context deadline and process-group kill; the connection closes with a documented status |
| GIT-PUSH-TIMEOUT | Push wall clock | 15 min | `git.push_timeout` | Same |
| GIT-STALL | No protocol progress | 60 s | `git.stall_timeout` | Progress detector on bytes read and written; the process is terminated |
| GIT-CONCURRENCY | Concurrent Git processes | `max(4, 2 x vCPU)`, cap 64 | `git.max_concurrency` | Admission control before spawning; over the limit returns `503` with `Retry-After`, never an unbounded queue |
| GIT-REPO-WRITER | Concurrent writers per repository | 1 | fixed | PostgreSQL advisory lock or a file lease; a second writer waits up to 5 s then receives `409` busy |
| GIT-CREDENTIAL | Concurrent Git operations per credential | 4 | `git.max_per_credential` | Admission control; over the limit returns `429` |
| GIT-MEMORY | Resident memory per Git process | 512 MiB | `git.memory_limit_bytes` | cgroup limit where available, plus `pack.windowMemory`, `pack.packSizeLimit`, `core.bigFileThreshold`, and unpack limits so a crafted repository cannot allocate unboundedly; a breach kills the process and reports a distinguishable outcome |
| GIT-SPOOL | Temporary pack spool per operation | 2 x the push cap | `GITNEST_TMP_ROOT` (startup) | Startup check fails when free space is below three times the push cap; spool removed on every exit path |
| GIT-REPO-SIZE | Repository size | 20 GiB soft refusal, warning at 5 GiB | `git.repository_max_bytes` | Pre-receive size estimate; refusal names the limit |
| GIT-LFS-OBJECT | LFS object | 2 GiB | `git.lfs_max_object_bytes` | Upload authorization |
| GIT-LFS-QUOTA | LFS bytes per repository | 50 GiB | `git.lfs_repository_bytes` | Upload authorization under a transaction lock |

1. Throughput is not itself a budget; time to first byte for clone and fetch is p95 at most 1 s on R1, after permission checks and ref advertisement.
2. `git clone` time and `git push` completion are reported per operation in the load tier; a change that moves them by more than 20% against the recorded baseline requires the §15 evidence.
3. No Git operation may hold a database transaction open while the Git process runs. The pattern is: authorize, reserve, run, then commit state with compare-and-swap.
4. A cancelled or client-disconnected operation must release its lock and remove its spool within the bounded cleanup deadline; a test asserts no orphan process and no leftover directory.

## 5. Diff and file-view caps

| ID | Limit | Default | Setting | Behaviour when hit |
| --- | --- | --- | --- | --- |
| DIFF-FILE-BYTES | Patch bytes per file | 2 MiB | `diff.max_file_bytes` | The file is listed with a too-large marker and no patch |
| DIFF-FILES | Files per page | 300 | `diff.max_files` | Further files load through the page's load-more path |
| DIFF-BYTES | Total diff bytes per pull request | 32 MiB | `diff.max_bytes` | Truncated with an explicit marker shared by the web page and the API |
| DIFF-LAZY | Lazy-load threshold | 400 KiB or 2,000 lines per file | `diff.lazy_threshold_bytes` | The file renders collapsed and loads on demand |
| DIFF-TIME | Diff computation per request | 10 s | `diff.time_limit` | The response is marked truncated or computing; full computation continues as a durable job |
| DIFF-RENAME | Rename detection | Enabled below 10,000 changed files | `diff.rename_max_files` | Rename detection is skipped with a note in the response |
| FILE-INLINE | Inline file render | 1 MiB of text | `files.inline_max_bytes` | The page links to raw and download views instead |
| FILE-IMAGE | Image preview | 10 MiB | `files.image_max_bytes` | Download link only |
| TREE-ENTRIES | Entries per directory listing | 1,000 | `files.tree_max_entries` | Paginated |
| COMMITS-PAGE | Commits per page | 100 | fixed | Pagination |
| DIFF-MEMORY | Concurrent files in memory per request | 4 files, 64 MiB | `diff.memory_bytes` | The differ streams files and releases each patch after serialization |

1. The web view and the API return identical truncation metadata for the same pull request; a test compares both responses.
2. A truncated diff is always labeled truncated and never presented as complete.
3. A crafted repository may not cause unbounded memory or time: diff computation reads only the objects within the file and byte caps, and the memory cap is verified in the load tier.

## 6. Search latency and result caps

| ID | Budget or cap | Value |
| --- | --- | --- |
| SEARCH-LATENCY-SQL | PostgreSQL full-text p95 on C1 | 400 ms |
| SEARCH-LATENCY-SQL-P99 | p99 | 1 s |
| SEARCH-LATENCY-EXT | External index p95, when configured | 250 ms |
| SEARCH-LAG | Index freshness p95 for a single repository push | 60 s; the SQL path has no lag |
| SEARCH-TIMEOUT | Statement timeout | 2 s, then `503` with the documented degraded body and `Retry-After` |
| SEARCH-RESULTS | Results per page, page or cursor | 100 |
| SEARCH-OFFSET | Page-pagination reach | 100 pages (10,000 results); beyond that a cursor is required |
| SEARCH-COUNT | Reported total | Capped at 1,000 and reported as `1000+` |
| SEARCH-QUERY | Query length, terms, qualifiers, nesting | 256 characters, 32 terms, 20 qualifiers, 3 nesting levels; a violation returns `422` naming the limit |
| SEARCH-SNIPPET | Snippet size | 2 KiB or 10 lines, escaped |
| SEARCH-FACETS | Facets and values | 10 facets, 20 values each |
| SEARCH-INDEX-FILE | Indexed file size | 1 MiB maximum; binary files excluded, exclusions documented and tested |
| SEARCH-INDEX-PATH | Indexed path length | 1,024 characters |
| SEARCH-BACKFILL | Backfill rate | At least 5 MiB/s per worker |

Every configurable value in this table is an instance setting under `search.*` (for example `search.max_results`, `search.query_timeout`, `search.index_max_file_bytes`); the remaining values are code constants.

1. Degradation: an unreachable external index falls back to SQL within 5 s of detection, every search response carries an explicit completeness indicator while degraded, and the degradation is logged once per window rather than per request.
2. Permission filtering happens before ranking, counting, and faceting. Facet totals and result counts are treated as disclosure surfaces and are covered by denied tests.
3. External index backfill lag is measured and exposed; a lag beyond the budget marks results incomplete rather than serving them as complete.

## 7. Archive generation limits

| ID | Limit | Default | Setting | Behaviour |
| --- | --- | --- | --- | --- |
| ARCHIVE-BYTES | Uncompressed output | 1 GiB | `archive.max_bytes` | Aborted before more bytes are written, with a documented error |
| ARCHIVE-ENTRIES | Entries | 100,000 | `archive.max_entries` | Aborted with the same error |
| ARCHIVE-PATH | Path length per entry | 1,024 characters | fixed | Entry skipped with a marker in the response metadata |
| ARCHIVE-TIME | Generation wall clock | 10 min | `archive.timeout` | Aborted; temporary spool removed |
| ARCHIVE-CONCURRENCY | Concurrent archive generations | 2 per instance, 1 per repository | `archive.concurrency` | Over the limit returns `429` with `Retry-After` |
| ARCHIVE-SPOOL | Temporary spool | At most the output cap | `GITNEST_TMP_ROOT` (startup) | Streamed where the format allows; zip spools then deletes on every exit path |

1. Symlinks and submodules are recorded as entries and never followed. `export-ignore` attributes are honored.
2. Output is deterministic: entry order and mtimes derive from the commit, so two identical requests produce identical bytes.
3. An optional cache is keyed by repository, ref, and format, with a 24-hour TTL, a size quota, and deletion by the prune job. A cache hit must be byte-identical to a fresh generation.
4. Archive generation never reads outside the requested tree and never buffers the whole archive in memory when the format allows streaming.

## 8. Markdown rendering limits

| ID | Limit | Default | Setting | Behaviour |
| --- | --- | --- | --- | --- |
| MD-BYTES | Input per document | 128 KiB | `markdown.max_bytes` | The first 128 KiB renders with an explicit truncation notice and a link to the raw source |
| MD-DEPTH | Block nesting depth | 32 | fixed | Deeper nesting is flattened with a notice |
| MD-TABLE | Table shape | 200 rows, 50 columns | fixed | Excess rows are truncated with a notice |
| MD-FENCE | Code fence | 256 KiB and 10,000 lines | fixed | Truncated with a notice; no syntax highlighting beyond the cap |
| MD-NODES | Inline nodes per document | 10,000 | fixed | Remaining inline content is rendered as plain text |
| MD-LINK | Link target length | 2,048 characters | fixed | Rejected and rendered as text |
| MD-TIME | Render time per document | p99 20 ms per 8 KiB; hard 250 ms | `markdown.time_limit` | The document is rendered as escaped plain text with a notice; the request is never blocked longer than the hard limit |
| MD-CACHE | Memoization | 10,000 entries and 64 MiB per process | `markdown.cache_bytes` | LRU eviction; the key is the document hash plus the renderer version |

1. Rendering runs under the request context deadline. Email rendering uses the same caps and the same renderer output.
2. Raw HTML stays disabled. The sanitizer allowlist is the only source of permitted elements, which is a security control and not a performance knob.
3. The hostile corpus in the unit tier (pathological emphasis, unterminated fences, deep lists, huge tables) must complete within the hard limit and stay within the cache and memory budgets.

## 9. Artifact, cache, and package blob limits and quotas

| ID | Item | Default | Setting | Enforcement |
| --- | --- | --- | --- | --- |
| REL-ASSET | Release asset size | 2 GiB | `releases.asset_max_bytes` | Rejected before bytes reach final storage; a partial upload leaves no orphan |
| REL-COUNT | Assets per release | 50 | `releases.assets_per_release` | Rejected |
| REL-REPO | Release asset bytes per repository | 20 GiB | `releases.repository_bytes` | Quota check under a transaction lock |
| ART-ASSET | Artifact size | 2 GiB | `artifacts.max_bytes` | Rejected before commit |
| ART-COUNT | Artifacts per run | 500 | `artifacts.max_per_run` | Rejected |
| ART-REPO | Artifact bytes per repository | 10 GiB | `artifacts.repository_bytes` | Quota check under a transaction lock |
| ART-RETENTION | Artifact retention | 90 days | `artifacts.retention_days` | Prune job |
| LOG-JOB | Job log size | 50 MiB | `logs.job_max_bytes` | Truncated with a marker in the body and the API response |
| LOG-CHUNK | Log chunk size | 1 MiB | fixed | Chunked upload |
| CACHE-ENTRY | Cache entry size | 2 GiB | `cache.entry_max_bytes` | Rejected |
| CACHE-REPO | Cache bytes per repository | 10 GiB | `cache.repository_bytes` | LRU eviction; a save that would exceed the cap is refused with a clear reason rather than evicting unrelated entries silently |
| CACHE-INSTANCE | Cache bytes per instance | 100 GiB | `cache.instance_bytes` | LRU eviction |
| CACHE-KEYS | `restore-keys` per request, prefix length | 10, 512 characters | fixed | Rejected |
| PKG-BLOB | Package blob size | 10 GiB | `packages.blob_max_bytes` | Rejected before final storage; digest verified before the version record is created |
| PKG-VERSION | Package version total bytes | 20 GiB | `packages.version_max_bytes` | Rejected |
| PKG-QUOTA | Bytes per owner | 50 GiB | `packages.owner_quota_bytes` | Quota check under a transaction lock before finalization, so two concurrent uploads cannot exceed the cap |
| WS-SNAPSHOT | Workspace snapshot size | 5 GiB | `workspaces.snapshot_max_bytes` | Rejected |
| STORAGE-RESERVE | Free-disk reserve | 20% of the volume | `storage.reserve_percent` | A write is refused when accepting it would breach the reserve |

1. Uploads write to a temporary path inside the storage root, verify a disk-space check against the declared or streamed size, and rename into place. An interrupted upload leaves no blob reachable from a version record.
2. Blob digests are verified on upload and again on a scheduled scrub; a mismatch quarantines the blob and reports it.
3. Quota accounting uses a reconciling job. A rejection names the exhausted limit and the current usage.
4. Retention and prune jobs delete in bounded batches per run so they cannot starve request traffic, and they report the rule that triggered each deletion.
5. Deleting a run, version, or repository removes its blobs, and a test asserts nothing remains reachable afterwards.

## 10. Runner and workspace resource limits

One runtime adapter serves both callers with different policies.

| Resource | Job default | Workspace default | Setting |
| --- | --- | --- | --- |
| CPU | 2 vCPU | 2 vCPU | `jobs.cpu`, `workspaces.cpu` |
| Memory | 7 GiB | 8 GiB | `jobs.memory_bytes`, `workspaces.memory_bytes` |
| Disk | 14 GiB | 32 GiB volume | `jobs.disk_bytes`, `workspaces.disk_bytes` |
| Process count | 512 | 512 | `jobs.max_pids`, `workspaces.max_pids` |
| Wall clock | 6 h maximum | 12 h maximum lifetime | `jobs.max_duration`, `workspaces.max_lifetime` |
| Step timeout | 60 min | not applicable | `jobs.step_timeout` |
| Idle timeout | not applicable | 30 min | `workspaces.idle_timeout` |
| Egress | Default deny, operator allowlist | Default deny, operator allowlist | `containers.egress_allowlist` |
| Egress bandwidth | 100 Mbit/s aggregate, 200 connections | Same | `containers.egress_bandwidth` |
| Log size | 50 MiB (shared with LOG-JOB) | Bounded server-side scrollback | `logs.job_max_bytes` |

1. Instance admission: jobs at most `max(2, vCPU)` up to a cap of 16 on R1; workspaces at most `max(2, vCPU / 2)` up to a cap of 8. The scheduler refuses admission beyond capacity minus a 20% reserve and never oversubscribes under concurrent admits.
2. Quotas: 2 concurrent jobs per user, 4 per repository, 2 concurrent workspaces per user; artifact, cache, and package byte quotas come from §9. Job minutes are unlimited by default on a self-hosted instance and are configured by the operator when they are not.
3. A quota rejection fails a new run with a distinguishable error naming the exhausted resource. Already running jobs are unaffected. Instance capacity, as opposed to quota, queues a workspace start and reports a queue position.
4. Idle and expired workspaces are stopped before a new start is refused.
5. Container images: pull only from the configured allowlist, at most 10 GiB per image, recorded by resolved digest, with a pull timeout and a 50 GiB local image cache under LRU eviction.
6. Prebuilds are limited to 5 per repository by default, with age and count pruning.
7. Every limit in this table is mandatory at the runtime boundary, not advisory in the scheduler: the container is created with the limits, and the control plane stops a workload that exceeds its lifetime.

## 11. Rate limits

| Class | Unauthenticated | Authenticated | Bucket |
| --- | --- | --- | --- |
| REST general | 60 per hour per source address | 5,000 per hour per credential | `core` |
| Search | 10 per minute | 30 per minute | `search` |
| GraphQL | Not served | 5,000 points per hour | `graphql` |
| Git clone and fetch | 100 per hour per source address | 5,000 per hour per credential | `git-read` |
| Git push | Not permitted | 500 per hour per credential, counted once per receive-pack request | `git-write` |
| Archive download | 10 per minute | 60 per minute | `archive` |
| Diff and file API | 60 per minute | 300 per minute | `diff` |
| Release and package asset download | 300 per minute per address | 5,000 per hour per credential | `download` |
| Registry read | 300 per minute per address, public packages only | 5,000 per hour per credential | `registry-read` |
| Registry write | Not permitted | 500 per hour per credential, 100 per minute burst | `registry-write` |
| Sign-in and invitation attempts | Failure-only: 10 failures per 15 minutes per account and address, 30 per 15 minutes per address | Successful authentication does not consume | `auth` |
| Webhook delivery | Not a client budget; at most one in-flight delivery per subscription, with the bounded retry schedule | | `webhook` |

Every budget in this table is an instance setting keyed `rate_limit.<bucket>.<tier>`, changed by an instance administrator through the operator console with an audit event and effective on the next evaluation.

1. Headers `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `X-RateLimit-Used`, and `X-RateLimit-Resource` appear on every response, authenticated or not.
2. A rejection returns `403` with `Retry-After` where a retry can succeed, and a machine-readable reason.
3. Counters are persisted, survive a restart, and are shared across every transport that authenticates the same credential: REST, GraphQL, Git over HTTP, Git over SSH, and registries draw on one budget.
4. Unauthenticated budgets are keyed by source address and never grant access to a private resource.
5. Per-account-type tiers (interactive user, machine user, installation token) are adjustable by organization or enterprise policy; a change is audited.
6. Bucket storage is bounded: counters expire with their window and the store has an entry cap with eviction, so a burst of unique keys cannot exhaust memory.
7. The webhook row is a delivery-concurrency budget, not a client rate limit; the retry schedule and the terminal failure state are defined in [testing.md](testing.md) §10.

## 12. Database query and connection-pool budgets

These values are startup configuration, not instance settings: pool sizing and timeouts differ per run mode (web, worker, migration), so they must be fixed before the process serves traffic.

| ID | Budget | Default | Setting |
| --- | --- | --- | --- |
| DB-POOL-WEB | Maximum open connections, web process | 20 | `GITNEST_DB_MAX_OPEN_CONNS` |
| DB-POOL-WORKER | Maximum open connections, worker process | 5 | `GITNEST_DB_MAX_OPEN_CONNS` |
| DB-IDLE | Maximum idle connections per process | 5 | `GITNEST_DB_MAX_IDLE_CONNS` |
| DB-ACQUIRE | Pool acquisition timeout | 2 s, then `503` with the documented error | `GITNEST_DB_ACQUIRE_TIMEOUT` |
| DB-LIFETIME | Connection maximum lifetime | 30 min | `GITNEST_DB_CONN_MAX_LIFETIME` |
| DB-IDLE-TIME | Connection maximum idle time | 5 min | `GITNEST_DB_CONN_MAX_IDLE_TIME` |
| DB-READ-TIMEOUT | Statement timeout, web read | 3 s | `GITNEST_DB_READ_TIMEOUT` |
| DB-WRITE-TIMEOUT | Statement timeout, web write | 5 s | `GITNEST_DB_WRITE_TIMEOUT` |
| DB-JOB-TIMEOUT | Statement timeout, background work | 60 s per statement, in bounded batches | `GITNEST_DB_JOB_TIMEOUT` |
| DB-LOCK-TIMEOUT | Lock wait on a request path | 2 s, then a busy `409` or `503` | `GITNEST_DB_LOCK_TIMEOUT` |
| DB-TX | Transaction duration | 1 s on a request path, 30 s in background work | `GITNEST_DB_TX_BUDGET` |
| DB-SLOW-LOG | Slow statement logging threshold | 500 ms | `GITNEST_DB_SLOW_QUERY_MS` |

1. Capacity formula: web + worker + administrative connections + monitoring must stay below `max_connections` minus 10. The formula is documented in `docs/operations/sizing.md`.
2. Pool limits are per process. A saturated pool fails the request rather than queueing past the acquisition timeout; pool wait above 100 ms increments `gitnest_db_pool_wait_seconds`.
3. A transaction is never held across a network call to a runner, a registry, or a webhook, and never across a Git process invocation.
4. There is no connection per request. One use case uses one transaction. Read replicas serve only the read-only query classes declared in W10.7; authorization, session, and read-after-write queries always use the primary.
5. Migration statements use `lock_timeout` of 5 s; a migration that must rewrite data states its expected duration before it runs.

## 13. Index policy prerequisites

1. Before a query lands, its plan is recorded with `EXPLAIN (ANALYZE, BUFFERS)` against corpus C1, and the change adds the index that makes the plan acceptable.
2. A request-path query may not sequentially scan a table with more than 10,000 rows in corpus C1.
3. Every list query filters by its scope (repository, organization, owner) before ordering and pagination. The index leads with the scope column, followed by the ordering column.
4. Keyset (cursor) pagination is required for any collection that can exceed 10,000 rows in corpus C1. Offset pagination is allowed below that with the reach cap in §6.
5. Uniqueness and idempotency constraints are indexes and are required: stars, reactions, votes, invitations, idempotency keys, slugs, and token digests.
6. Every foreign key has a supporting index unless a recorded plan proves the query does not need one.
7. State-filtered queries use partial indexes rather than a leading low-cardinality column.
8. Search uses generated `tsvector` columns with GIN indexes, plus `pg_trgm` for prefix matching. JSONB uses GIN only where queried.
9. A low-cardinality column is never indexed alone; it is part of a composite or partial index.
10. High-churn tables (job queue, notifications, webhook deliveries, job logs, traffic counters) have their own autovacuum settings and a documented retention before any index is added.
11. Index count is reviewed for write amplification: a table written more than 1,000 times per second in the load profile carries at most five indexes unless a measurement justifies more.
12. Every index in the table below has a test that fails when the index is dropped or the plan stops using it, so an index cannot be removed silently.

| Query family | Required index shape | Verification |
| --- | --- | --- |
| Repository list by owner, visibility, recent activity | `(owner_type, owner_id, visibility, updated_at DESC)` | Plan test on C1; no sequential scan |
| Repository lookup by owner and name | Unique `(owner_type, owner_id, name)` | Uniqueness test plus plan test |
| Issue list by repository and state | Partial `(repository_id, updated_at DESC) WHERE state = 'open'` | Plan test |
| Issue and pull-request number allocation | Unique `(repository_id, number)` | Concurrency test |
| Pull-request list by repository and state | `(repository_id, state, updated_at DESC)` | Plan test |
| Comments by thread and creation order | `(thread_id, created_at, id)` | Plan test |
| Notification inbox, unread first | Partial `(user_id, updated_at DESC) WHERE read_at IS NULL` | Plan test |
| Job queue claim | Partial `(state, run_at) WHERE state = 'ready'`, claimed with `FOR UPDATE SKIP LOCKED` | Claim concurrency test |
| Outbox undelivered | Partial `(created_at) WHERE delivered_at IS NULL` | Plan test |
| Audit log by organization and time | `(organization_id, created_at DESC)` | Plan test |
| Audit log by actor and action | `(organization_id, actor_id, action, created_at DESC)` | Plan test |
| Sessions and personal access tokens | Unique `(token_digest)`, unique `(token_hash)` | Lookup plan test |
| Repository permission lookup | `(repository_id, principal_type, principal_id)` | Plan test |
| Stars and watchers | Unique `(user_id, repository_id)` plus `(repository_id)`, `(repository_id, watcher_state)` | Count plan test |
| Search documents | GIN on the generated `tsvector` plus the scope column | Search plan test |
| Packages by owner and name | Unique `(registry_kind, owner_type, owner_id, name)` | Uniqueness and lookup tests |
| Package versions by package | `(package_id, published_at DESC)` | Plan test |
| Check runs by head commit and name | `(head_sha, name)` | Required-check plan test |
| Workspaces by owner and state | `(owner_id, state)` | Plan test |

## 14. Budget to measurement map

| Budget | Metric or log field | Test |
| --- | --- | --- |
| PAGE-* | `gitnest_http_request_duration_seconds{route_class, method, status}`; log fields `route_class`, `duration_ms`, `query_count` | T6 page budget check; T7 report |
| API-* | Same metric for API route classes; `gitnest_http_response_bytes` | T2 table tests for status; T7 report |
| Query count | `gitnest_http_db_queries{route_class}`; log field `query_count` | T2 counting-driver test |
| WEB-* | Bundle sizes from `web/scripts/check-bundle-size.mjs`; vitals from the browser tier report | T1W check plus T6 measurement |
| GIT-* | `gitnest_git_operation_duration_seconds{op,outcome}`, `gitnest_git_rejected_total{reason}`, `gitnest_git_process_memory_bytes`, `gitnest_git_concurrency` | T3 conformance; T7 report |
| DIFF-* | `gitnest_diff_duration_seconds`, log fields `truncated`, `limit_reason`; response field `truncated` | T2 web-versus-API metadata comparison; T7 memory check |
| FILE-* | `gitnest_file_render_bytes` | T2 |
| SEARCH-* | `gitnest_search_duration_seconds{backend}`, `gitnest_search_degraded_total`, `gitnest_search_index_lag_seconds` | T2 fallback test; T7 latency report |
| ARCHIVE-* | `gitnest_archive_duration_seconds{outcome}`, `gitnest_archive_bytes_total`, `gitnest_archive_rejected_total{reason}` | T2 cap tests; T3 determinism test |
| MD-* | `gitnest_markdown_render_duration_seconds`, `gitnest_markdown_cache_total{result}` | T1 hostile corpus; T7 |
| REL-*, ART-*, LOG-*, CACHE-*, PKG-* | `gitnest_blob_bytes{kind}`, `gitnest_quota_rejected_total{kind,reason}`, `gitnest_storage_bytes{kind}` | T2 quota and concurrency tests; T5 client conformance |
| Runner and workspace limits | `gitnest_job_duration_seconds`, `gitnest_job_failed_total{reason}`, `gitnest_workspace_lifecycle_seconds{state}`, `gitnest_runner_lease_expired_total`, `gitnest_admission_rejected_total{reason}` | T4 isolation and admission tests |
| Rate limits | `gitnest_rate_limit_rejections_total{bucket,tier}`, `gitnest_rate_limit_in_use{bucket}` | T1 bucket tests; T2 restart persistence test |
| DB-* | `gitnest_db_pool_in_use`, `gitnest_db_pool_wait_seconds`, `gitnest_db_query_duration_seconds{class}`, `gitnest_db_lock_wait_seconds` | T2 statement-count and lock tests; T7 report |
| Storage reserve | `gitnest_disk_free_bytes`, `gitnest_storage_reserve_breaches_total` | T2 rejection test |
| Index policy | `gitnest_db_seq_scan_total{table}` plus recorded plans | T2 plan tests per §13 |

Log fields that carry a budget always include the measured value and the configured limit, never a user name, repository name, token, or file path.

## 15. Raising a budget

A budget may be lowered at any time. Raising one requires all of the following, recorded in this document:

1. A written change with the old value, the new value, the date, the reason, and the maintainer's approval. A shipped default that changes without a row here is a defect.
2. Evidence: a load-tier report on the named reference class and corpus showing the measured value against the current budget, plus a profile (CPU, heap, query plan, or trace) that identifies where the cost is.
3. A rejected alternative: the change states why optimizing, caching, paginating, chunking, or splitting the surface was insufficient.
4. A security note when the budget is also a limit (size, time, memory, concurrency, egress, quota): the worst-case resource use at the new value, the blast radius, and any compensating control.
5. Simultaneous updates: this document, the registry default for an instance setting or the default in `internal/config` for a startup variable, the constant the test reads, `docs/operations/sizing.md`, and the README configuration table when the value is a startup variable.
6. Two consecutive monthly load runs at or below the new value before the previous number is removed from this document.
7. A test may never be edited to raise a budget. The test reads the same constant the production default uses, and only that constant changes.
8. An operator changes an instance setting through the operator console for capacity reasons; the change is audited and does not change the shipped default. A startup variable is raised through the documented configuration and restart procedure. Changing a shipped default follows this section.
9. A budget raised twice without a corpus or design change triggers a redesign review of that surface.
10. An emergency increase during an incident is allowed as a configuration-only change, is recorded in the incident record, and expires after 30 days unless ratified under this section.
11. Security invariants are not budgets and cannot be raised here: raw HTML remains disabled in rendered Markdown, untrusted execution never runs in the web process and never receives a database credential, authorization is never cached across a request boundary, and no limit may be disabled entirely (a value of `0` meaning unlimited is not a valid configuration for a size, time, memory, or concurrency limit).
