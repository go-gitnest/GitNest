# Testing strategy

**Status:** Target design. The repository currently contains documentation only; nothing described here is implemented. Every requirement below is a specification for the rebuilt codebase.
**Scope:** Test tiers, environment gates, CI jobs, isolation and protocol conformance, fixtures, concurrency, flakiness, and the test kinds required at each boundary.
**Related:** [performance.md](performance.md) defines every limit these tests assert against.

## 1. Principles

1. A behaviour is proven at the narrowest tier that can prove it. A higher tier proves integration, never domain logic twice.
2. Every permission-bearing feature has an allowed test and a denied test at the shared application service, not only at an HTTP status code.
3. Every limit in [performance.md](performance.md) has a test that reaches the limit and a test just below it.
4. No tier passes by skipping silently. A skip names its gate; §3 defines how a skip becomes a failure.
5. Untrusted input is tested with hostile fixtures as well as valid ones.
6. Tests are order-independent. `t.Parallel` is used only by a test that owns its schema, temp root, and port.
7. No test waits with an unconditional sleep.
8. No test reaches the public internet, uses production data, or depends on a developer's ambient environment.

## 2. Test tiers

| Tier | Name | Proves | Gate | Runs |
| --- | --- | --- | --- | --- |
| T1 | Go unit | Domain rules, parsers, validators, expression evaluation, scheduling arithmetic, policy decisions, error mapping | none | every change |
| T1W | Frontend unit and component | React component behaviour, hooks, client state, form validation | Node.js toolchain | every change |
| T2 | PostgreSQL integration | Migrations, persistence, transactions, locks, constraints, service use cases, durability | `GITNEST_TEST_DATABASE_URL` | every change |
| T3 | Git-transport conformance | Smart HTTP and SSH against a real `git` client: clone, fetch, push, LFS, archive, ref updates, cancellation | T2 gate plus a `git` executable | every change |
| T4 | Container and runtime | Runner jobs, service and action containers, workspace lifecycle, isolation assertions, cleanup | `GITNEST_TEST_CONTAINER_RUNTIME` plus the T2 gate where persistence is involved | main branch and nightly; also on a pull request when a runtime is present |
| T5 | Package-registry conformance | OCI, npm, Maven, NuGet, RubyGems, Composer, Helm, and generic registries against their native clients | `GITNEST_TEST_REGISTRY_CLIENTS` plus the T2 gate | nightly and before a release |
| T6 | Browser end-to-end | Real browser flows, keyboard-only paths, rendering and escaping, web performance budgets | `GITNEST_TEST_E2E_BASE_URL` (or an in-test server) plus `GITNEST_TEST_E2E_BROWSER` | main branch and nightly |
| T7 | Load and soak | The budgets in [performance.md](performance.md) on the reference instance class | `GITNEST_TEST_LOAD_TARGET` plus `GITNEST_TEST_LOAD_PROFILE` | monthly and before a release, against staging |
| T8 | Identity-provider conformance | SAML SSO and SCIM against a stub IdP in-process, and optionally against a real IdP | stub IdP: T2 gate; real IdP: `GITNEST_TEST_IDP_BASE_URL` | stub on every change; real IdP nightly |

A tier is a directory of tests, not a build tag. A test that belongs to a tier but has no external dependency runs as T1 and is not skipped.

## 3. Environment gates

Only variables with the `GITNEST_TEST_` prefix may be read by a test. Application configuration is constructed explicitly in the test, never read from the ambient environment.

| Variable | Tier | When unset |
| --- | --- | --- |
| `GITNEST_TEST_DATABASE_URL` | T2, T3, T4, T5, T6, T8 | The test calls `t.Skipf` naming this variable. `internal/platform/postgrestest` refuses to hand out a schema without it. |
| `GITNEST_TEST_REQUIRE` | all | Comma-separated tier names that may not skip. A missing gate for a required tier fails in `TestMain` before any test body runs, so a required tier cannot report success by skipping. |
| `GITNEST_TEST_DB_CONCURRENCY` | T2 | Default 8. Caps how many test schemas exist in parallel so parallel packages cannot exhaust `max_connections`. |
| `GITNEST_TEST_GIT_BIN` | T3 | Default `git` resolved from `PATH`. If it is absent and T3 is required, the tier fails rather than skipping. |
| `GITNEST_TEST_SSH_ADDR` | T3 | The test starts an SSH listener on an ephemeral loopback port itself. A set value is used as-is. |
| `GITNEST_TEST_SERVER_URL` | T3, T5, T6 | The test builds and starts the binary on an ephemeral loopback port with a fresh schema and temp storage root. |
| `GITNEST_TEST_ALLOW_REMOTE` | T3, T5, T6 | Unset. A non-loopback `GITNEST_TEST_SERVER_URL` is refused unless this is `1`. |
| `GITNEST_TEST_CONTAINER_RUNTIME` | T4 | Skip the runtime tier. Values: `podman`, `docker`, `remote`. |
| `GITNEST_TEST_CONTAINER_ENDPOINT` | T4 | Required only for `remote`; otherwise the local socket is used. |
| `GITNEST_TEST_REGISTRY_CLIENTS` | T5 | Skip the client tier. Comma list drawn from `docker,podman,npm,mvn,gradle,dotnet,gem,composer,helm,curl`. A listed client whose binary is missing fails the tier; the list is an assertion, not a wish. |
| `GITNEST_TEST_E2E_BROWSER` | T6 | Skip the browser tier. Values: `chromium`, `firefox`, `webkit`. |
| `GITNEST_TEST_E2E_BASE_URL` | T6 | The test starts its own instance. |
| `GITNEST_TEST_LOAD_TARGET` | T7 | Skip. Refused for a non-loopback target without `--confirm-writes`; refused for a release build unless `GITNEST_TEST_LOAD_ALLOW_RELEASE=1`. |
| `GITNEST_TEST_LOAD_PROFILE` | T7 | Skip; there is no default profile. |
| `GITNEST_TEST_LOAD_SEED` | T7 | Skip; the corpus seed is explicit so a run is reproducible. |
| `GITNEST_TEST_IDP_KIND` and `GITNEST_TEST_IDP_BASE_URL` | T8 | Skip the real-IdP interop run. The in-process stub IdP needs no gate beyond T2. |
| `GITNEST_TEST_WAIT_SCALE` | all | Default 1. Integer 1 to 4; multiplies bounded-wait deadlines only. It never introduces a sleep and never lengthens a production timeout. |
| `GITNEST_TEST_SECRET_KEY` | T4, T5 | A fixed 32-byte test key is used. `internal/config` must refuse the documented test key outside tests, and a test asserts that refusal. |

Rules:

1. An unset gate produces `t.Skipf("<variable> not set: <tier> tier skipped")`. A bare `t.Skip()` is a defect.
2. `GITNEST_TEST_REQUIRE` converts a skip into a failure. CI sets it for the release-blocking tiers on the main branch and on the nightly run.
3. A skip for any reason other than an unset gate names the missing artefact (a fixture, a corpus, a client binary). A missing artefact that the tier claims to cover is a failure, not a skip.
4. Tests never read `GITNEST_DATABASE_URL`, `GITNEST_PUBLIC_URL`, or any other application variable from the ambient environment; they pass an explicit environment slice to the code under test.
5. A test never writes outside its schema, its `t.TempDir()`, or its container.
6. Remote targets are refused by default. T7 additionally requires explicit confirmation before any write scenario.

## 4. CI jobs

CI is defined in `.github/workflows/ci.yml`. Build, packaging, asset-freshness, and documentation jobs are inventoried in [packaging.md](packaging.md) §9; the table below is only the subset of jobs that runs a test tier.

| Job | Tiers | Runtime | Required on a pull request |
| --- | --- | --- | --- |
| `go-test-unit` | T1 | Go 1.26 | yes |
| `web-unit-test` | T1W | Node.js with a frozen lockfile | yes |
| `go-test-integration` | T2, T8 (stub IdP) | PostgreSQL service container; `go test -race` with `GITNEST_TEST_DATABASE_URL` and `GITNEST_TEST_REQUIRE=postgres` | yes |
| `git-transport` | T3 | Go, `git`, and an SSH listener in-process | yes |
| `containers` | T4 | Rootless Podman installed in the job (§7) | no; required on main and nightly with `GITNEST_TEST_REQUIRE=container` |
| `registries` | T5 | Client binaries installed per matrix entry | no; nightly and release |
| `web-e2e-keyboard` | T6 | Playwright Chromium, built web assets | no; main and nightly |
| `idp-real` | T8 | Containerised IdP | no; nightly |
| `load` | T7 | Staging instance | no; monthly and before a release |

Rules:

1. CI does not automatically retry a failed Go test. A retry hides a flake, and §16 requires flakes to be visible.
2. `-race` is mandatory in every job that runs T2, T3, T4, T5, or T8.
3. Each job prints the number of tests skipped and the reason, so an accidental gate change is visible in the log.
4. `GITNEST_TEST_REQUIRE=container` means a runtime-less environment fails the job instead of skipping it. The one deliberate gap is a pull request from a fork that cannot obtain a runtime: the container job is not required there, and the release checklist requires a green container run on the release commit.
5. The T7 job is never allowed to report success without a report file; a missing report is a failure.

## 5. PostgreSQL integration tests

1. Every test that touches persistence obtains a schema from `internal/platform/postgrestest`. The helper creates a uniquely named schema, applies `migrations/` through the real migration runner, and drops the schema in `t.Cleanup`. It never drops a database.
2. Each test asserts its own isolation: a schema-scoped query must not see rows created by another test running in parallel.
3. Per-test pools are bounded (at most 4 connections). `GITNEST_TEST_DB_CONCURRENCY` caps concurrent schemas.
4. Migrations are applied by the production runner, not by test-local DDL, except in migration tests that assert the ledger, forward-only ordering, per-migration transaction, and rollback of a failed migration.
5. Migration tests cover: apply twice with the second a no-op; a failed migration rolling back both schema and ledger row; two concurrent runners applying each migration exactly once; a database newer than the binary failing preflight; the dry-run and status commands.
6. Identifier handling tests store and return names containing quotes, semicolons, newlines, NUL, and non-BMP Unicode byte-identically, proving parameterized queries.
7. Every service test that mutates permissions asserts the denied path for each principal kind (anonymous, read, write, admin, owner, machine user, deploy key, installation token).
8. Time-dependent persistence tests receive an injected clock; a stored timestamp is asserted against the injected value, not against `time.Now`.

## 6. Git-transport conformance

All T3 tests drive the real `git` executable as a child process against a running GitNest instance and assert outcomes on disk with Git itself where possible. `git fsck --strict` on the bare repository is the integrity oracle after every mutating case.

| Operation | Client invocation | Assertions |
| --- | --- | --- |
| Anonymous clone of a public repository | `git clone <public-url>` | Clone succeeds; the advertised URL derives from `GITNEST_PUBLIC_URL` and not from the request `Host` header |
| Anonymous clone of a private repository | `git clone <private-url>` | Authentication is required; the response body and status are identical to those for a nonexistent repository; existence is not disclosed |
| Clone with a personal access token | Credential helper or `http.extraHeader` | Clone succeeds; captured logs contain no token, no `Authorization` header, and no pack bytes |
| Protocol v2 | `GIT_TRACE_PACKET` capture | The `version 2` capability line is present and the client issues `ls-refs` and `fetch` |
| Shallow clone | `git clone --depth=1` | Succeeds; the shallow boundary is honored; the server does not send full history |
| Filtered clone | `git clone --filter=blob:none`, when advertised | The documented advertisement matches behaviour; when not advertised the client falls back cleanly |
| Incremental fetch | `git fetch` after a push | Only new objects transfer; the ref updates to the pushed commit |
| Push: new branch, update, delete, tags | `git push` variants | Ref state matches; an annotated tag keeps its tag object; deletion removes only the requested ref |
| Force push without lease | `git push --force` | Rejected where policy forbids it; no ref change |
| Push to a protected ref | `git push` to a protected branch | Rejected by the shared rule evaluation with a machine-readable reason; no ref change; the audit record exists |
| Non-fast-forward push | `git push` after a diverging commit | Rejected; no ref change; the repository passes `git fsck` |
| Oversized push | A push above the configured pack cap | Rejected before unpacking, with the documented status; the temporary spool is removed |
| Ref cap | A push exceeding the maximum refs per operation | Rejected with the documented error; the ref transaction is all-or-nothing |
| Path traversal in the URL | Requests with `..`, extra segments, absolute-looking paths | Not-found response; no filesystem effect outside the repository root |
| Concurrent pushes to one ref | Two pushes raced through a barrier | Exactly one wins; the loser gets a moved-ref error; `git fsck` passes |
| Client abort mid-transfer | Kill the client during clone and during push | The Git process is reaped within the bounded deadline; the lock is released; no orphan process, socket, or temporary directory remains |
| SSH key authentication | `git clone` and `git push` over SSH | Only `git-upload-pack` and `git-receive-pack` are dispatched |
| SSH restrictions | Any other command, PTY request, agent forwarding, port forwarding, `scp` path | Refused; no shell starts; key material never appears in logs |
| Revoked or unknown key | SSH clone with a revoked key | Authentication fails; the attempt is not counted as a repository access |
| Git LFS | `git lfs push` and `git lfs pull` | Batch API round trip; an unauthorized caller cannot fetch an object by hash; the size limit is reported clearly |
| Archive | The archive endpoint for a ref, tar.gz and zip | Entry list and blob hashes match `git archive` for the same ref; symlink entries are recorded and never followed; two identical requests produce identical bytes |
| Repository integrity | `git fsck --strict` after every mutating case | No dangling corruption, no broken ref |

## 7. Container and runtime tests

### 7.1 Options compared

| Option | Isolation fidelity | Setup cost | Failure modes it hides | Verdict |
| --- | --- | --- | --- | --- |
| Rootless container execution on the CI runner | Matches the intended production posture: no root daemon, user namespace, cgroup limits applied by the caller | Requires cgroup v2, subuid/subgid mapping, and a runtime that supports rootless mode | None specific, but nested-rootless is limited and some kernels restrict it | Default |
| Docker-in-docker | Tests a daemon the product does not require and usually needs privileged mode | A privileged container or sidecar, seconds of startup, its own storage driver | Hides user-namespace and cgroup-delegation failures that a rootless install hits | Compatibility job only, for the Docker-compatible adapter path |
| Remote runner host | The only option that proves outbound-only registration, label matching, lease expiry across hosts, and multi-node admission | A second host, network reachability, provisioned credentials | Local-only tests never exercise a real network partition or a real agent restart | Nightly, for registration, label, lease, and multi-node cases only |

### 7.2 Chosen default and fallback

1. The default runtime for T4 is rootless Podman installed in the CI job, `GITNEST_TEST_CONTAINER_RUNTIME=podman`. The runtime adapter contract suite runs against it.
2. A second, non-default job runs the same suite with `GITNEST_TEST_CONTAINER_RUNTIME=docker` against the job's own daemon where one exists, to prove the adapter contract holds for both backends. It is not a compatibility substitute for the rootless job.
3. The remote host is used only where a second host is the point: runner registration and scope, label matching, lease expiry after an agent kill, and multi-node admission.
4. When no runtime is available, container-dependent tests skip and say so; with `GITNEST_TEST_REQUIRE=container` the tier fails instead.
5. Everything decidable without a container is proven against a fake `Runtime` that implements the same interface: cleanup ordering, label-based orphan reconciliation, quota arithmetic, admission control, lease expiry, and the no-runtime capability probe. These tests are T1 or T2 and are never skipped.
6. A test asserts the degraded path: with no runtime configured, a container job and a workspace create both fail with an explicit, actionable error and never fall back to executing on the host.

### 7.3 Capability probe

1. `Runtime.Probe(ctx)` reports rootless support, cgroup version, seccomp availability, user-namespace support, and network-policy support.
2. A test asserts the probe distinguishes "no runtime configured" from "runtime configured but unhealthy".
3. A test may skip only the specific capability it names, never a whole tier, and the skip message names the capability.

## 8. Untrusted execution isolation tests

Each row is an assertion, a method, and the tier that runs it. Jobs and workspaces are tested separately because they have different policies over one runtime adapter.

| Assertion | Method | Tier |
| --- | --- | --- |
| No host mount | Inspect the created container's mount list; it must equal the expected run-scoped set (workspace volume, checkout, allowlisted cache root). Inside the container, probes assert the repository storage root, the instance configuration directory, the runtime socket, and `/etc/gitnest` are absent | T4 |
| No runtime socket | In-container existence check for `/var/run/docker.sock` and the Podman socket path | T4 |
| No credentials | The injected environment must equal an explicit allowlist. A database connection attempt from inside fails or times out. No `GITNEST_*` value, secret key file, cloud credential, or instance token is present | T4 |
| No metadata reachability | A request to the link-local metadata address is blocked by the default-deny egress policy unless the operator allowlisted it | T4 |
| CPU limit | A busy loop's measured CPU share stays within the limit plus tolerance | T4 |
| Memory limit | An allocation above the limit is killed by the container, the job reaches a distinct conclusion, and the host stays healthy | T4 |
| Process limit | A fork-bomb fixture is contained; the host process count is unaffected | T4 |
| Disk limit | Writing beyond the disk or volume quota fails with a clear error; host disk is not exhausted | T4 |
| Wall-clock limit | A job exceeding its timeout has its whole process tree terminated; no orphan process survives; the lease is released | T4 |
| Deterministic cleanup on completion and failure | Enumerate containers, networks, and volumes by run label before and after; assert equality | T4 |
| Deterministic cleanup on cancel | Cancel a running job; assert a terminal state within the bounded deadline and no leftover container, network, or temp directory | T4 |
| Deterministic cleanup on crash | `SIGKILL` the worker mid-job, restart it, run the reconciler, and assert exactly the orphans are removed and a second reconciler run is a no-op | T4 |
| Secret masking | A secret printed in plain, base64, JSON-escaped, multi-line, and URL-encoded forms is absent from stored logs, streamed logs, artifacts, and step summaries | T1 plus T4 |
| Fork pull request | Zero repository secrets resolve; the workflow token is read-only and a write through it is denied by the service; no environment secret is injected | T4 |
| Host execution is opt-in | With default configuration, a job requesting host execution is refused. The test asserts the default is off | T1 plus T4 |
| Workspace non-root | The workspace process user is not root and capabilities are dropped | T4 |
| Workspace cross-tenant isolation | A volume, image, or snapshot identifier belonging to another workspace cannot be attached through any API input | T2 plus T4 |
| Workspace egress policy | Egress to an unlisted host fails; egress to an allowlisted host succeeds | T4 |

## 9. SAML SSO and SCIM against a test identity provider

1. `internal/identity/samltest` is an in-process IdP test double. It generates a signing key at test start, serves metadata, supports SP-initiated and IdP-initiated flows, and can be instructed to tamper with a response. It is deterministic: assertion IDs, instants, and session indexes come from the test clock and an explicit seed.
2. `internal/organization/scimtest` is a SCIM 2.0 client driver and a fake provisioning source. GitNest's own SCIM server is validated with this independent client, so the server is not tested with its own request builder alone.
3. Assertions must use an injected clock. `NotBefore` and `NotOnOrAfter` are produced from that clock and the test advances it instead of waiting.

| Case | Expected outcome |
| --- | --- |
| Valid signed assertion, SP-initiated | Session created; mapped username and email stored; JIT member provisioned with the mapped least privilege |
| Valid signed assertion, IdP-initiated | Session created through the documented entry point only |
| Unsigned assertion | Rejected |
| Signature from an unpinned certificate | Rejected |
| Expired assertion | Rejected |
| Not-yet-valid assertion | Rejected at both clock-skew edges |
| Replayed assertion ID | Rejected |
| Wrong audience, wrong recipient, wrong ACS URL | Rejected, each with a distinguishable reason in the audit record |
| Email outside the associated domain | Rejected; no account is created |
| Attribute mapping variants | Username, email, display name, and role mapping asserted for each documented mapping |
| IdP-side revocation | Organization-scoped access is lost on the next request; the verified session is invalidated |
| Maximum SSO session lifetime | Enforced at the documented bound |
| SCIM create then login | The provisioned user can sign in; the deprovisioned user cannot |
| SCIM deactivate | Sessions and repository access are revoked on the next request; commit authorship is preserved |
| SCIM group-to-team mapping and unmapping | Team membership follows the mapping; an unmapped group is reported, not ignored |
| SCIM repeated delivery | Idempotent; no duplicate membership or role change |
| SCIM token scope | A token for one organization cannot affect another |
| SCIM role escalation | Clamped to the organization owner's own grant |

4. Assertions never appear in a log line, an error response, or an audit detail field. A log-capture test asserts the assertion XML is absent.
5. Assertion size, attribute count, and SCIM payload size are bounded; a test submits an oversized assertion and a deeply nested SCIM patch and asserts a documented rejection.
6. When `GITNEST_TEST_IDP_KIND` and `GITNEST_TEST_IDP_BASE_URL` are set, the nightly job repeats the provisioning, login, group mapping, and deactivation cases against a real IdP. Unset means the interop run is skipped and named as skipped.

## 10. Webhook delivery, retries, and signature verification

1. The sink is an in-process `httptest.Server` on loopback. Because the outbound policy blocks loopback by default, the test registers its sink in the test allowlist. A separate test asserts that without that entry the delivery is refused, which is the SSRF control's proof.
2. Signature: `X-Hub-Signature-256` is HMAC-SHA256 over the raw transmitted bytes. The sink recomputes the digest from the received body and compares in constant time. A tampered body, a wrong secret, and a truncated body are rejected. The SHA-1 compatibility path is off by default, and a test asserts that enabling it is recorded in the delivery record.
3. Retry and backoff: the sink is scripted with a sequence (500, timeout, connection reset, 429, 404). Assertions:
   - 5xx, timeout, and connection errors retry; 4xx does not.
   - The attempt count is capped and the terminal state is recorded with the last error.
   - The backoff schedule matches the documented bounds; the schedule is asserted against the injected clock and scheduler, never by waiting.
   - Jitter stays within the documented bounds when the jitter source is seeded.
4. Delivery records: one record per attempt; the delivery GUID is stable across redelivery; the stored response prefix is bounded; redelivery reproduces a byte-identical signed payload with the same GUID; a scan of the stored record finds no subscription secret, token, or payload credential.
5. Durability: killing the worker between enqueue and delivery loses no delivery and produces exactly one delivery after restart. A duplicate enqueue produces one delivery under the idempotency key. Failing the outbox insert rolls back the domain write.
6. Egress controls: a redirect response is not followed; a sink that streams forever is read only to the cap and the connection is closed; a resolver double that returns a public address on the first lookup and a private one on the second causes a refusal; the per-delivery timeout is enforced.
7. Each event in the webhook catalogue has a payload fixture validated against the published schema and the API version, and a test asserts a private repository's event is never delivered to a subscription lacking access.
8. Delivery ordering for one subscription is preserved by keeping one in-flight delivery per subscription, and a test asserts that property under a slow sink.

## 11. Package-registry conformance

Each registry kind runs two suites: a client-independent contract suite (§17.2) and a native-client suite below. Native clients run with a temporary `HOME` and cache directory so a developer's real cache cannot produce a false pass, and every client case asserts a corresponding server access record to prove the client contacted GitNest.

| Registry | Client | Cases |
| --- | --- | --- |
| OCI / Docker | `docker`, `podman` | Push and pull a multi-layer image; verify the digest; pull into a clean daemon; push a manifest whose layer digest is absent is rejected with the documented body; private pull without a credential is denied; chunked and monolithic blob upload |
| npm | `npm publish`, `npm install` | Publish, install in a clean directory, resolve a dist-tag; republish over an existing version is rejected; the served tarball integrity matches the recorded field; scoped name maps to the owner namespace |
| Maven / Gradle | `mvn deploy`, Gradle `maven` | Deploy a jar with checksums and resolve it from a clean cache; a checksum mismatch on an existing artifact is rejected; a duplicate release version is rejected; snapshot uniqueness |
| NuGet | `dotnet restore`, `dotnet nuget push` | Restore from a clean cache; duplicate ID and version push is rejected with the documented status; case-insensitive lookup resolves the same package; symbol package upload |
| RubyGems | `gem push`, `gem install --source` | Push and install; compact index lists every version in publish order; a yank removes the version from the index while the exact-URL download rule holds |
| Composer | `composer require` | Resolution and dist download from a clean cache; metadata shape asserted |
| Helm | `helm pull`, `helm push` | Chart pull over the generic transport; the OCI path uses the container registry |
| Generic | `curl` | Byte-preserving round trip with a digest assertion; the same name in two registry kinds does not collide |

Every registry case is repeated without a credential and with an insufficient scope; both must be denied with a documented response that does not disclose existence.

## 12. Browser and end-to-end tests

1. The driver is a pinned browser automation tool under `web/e2e/`. It is a development dependency: `go build ./...` and serving the application must not require Node.js.
2. The test instance is the built binary plus a fresh schema, a temporary repository root, a fake mail transport, and an in-process webhook sink, started on an ephemeral loopback port and stopped with a bounded deadline.
3. Minimum browser paths, one per parity area:

| Area | Minimum path |
| --- | --- |
| Identity | Sign in, second-factor challenge, sign out, invitation acceptance |
| Repository | Create, push from a fixture client, browse tree, view a file, view commits, clone URL |
| Issues | File with a template, comment, label, assign, close, reopen |
| Pull requests | Open a draft, mark ready, view files, comment on a line, approve, merge (merge, squash, rebase) |
| Actions | Trigger a workflow, watch a run, read a log, download an artifact |
| Packages and releases | Cut a release with an asset, download it, list a package and its versions |
| Search | Query with qualifiers, page through results, see no private content |
| Administration | Change a collaborator role, branch protection, audit log query |
| Cloud development | Start a workspace, open the IDE and a terminal, forward a port, stop and resume (T4 required) |

4. Accessibility: each path has a keyboard-only variant; focus order after each navigation is asserted; an automated accessibility scan supplements but never replaces the keyboard test.
5. Escaping: a fixture repository contains file content and Markdown that attempt script injection. The test asserts no injected element is created and no injected handler runs, and that repository text appears only as text.
6. Web performance: the browser tier measures TTI, LCP, INP, and CLS on the reference profile defined in [performance.md](performance.md) and fails when a budget is exceeded by more than its tolerance. `web/scripts/check-bundle-size.mjs` enforces the bundle budgets in the same job.
7. Visual snapshots are used only where layout is the contract. They follow the golden-file rules in §14.

## 13. Load and soak tests

1. The corpus generator `internal/loadtest/corpus` is deterministic from a seed and produces the reference corpus described in [performance.md](performance.md).
2. Scenarios: clone-heavy, push-heavy, browse-heavy, many small repositories against few large ones, many concurrent workflow jobs, and many concurrent workspaces. Each scenario declares the instance limit it is expected to reach first.
3. Safety: the harness refuses a non-loopback target without an explicit confirmation flag, refuses a target reporting a release build unless explicitly allowed, bounds its own concurrency, and prefers read-only scenarios.
4. Output is a report comparing each measured value against the corresponding budget row in [performance.md](performance.md). A deliberate regression injected through a test double must fail the comparison.
5. Each report records the instance class, the corpus manifest, the commit, and the date, so two runs are comparable. Baselines are recorded per instance class.
6. A 24-hour soak run asserts bounded growth of goroutines, file descriptors, temporary directories, containers, and pool connections; an unbounded slope fails.

## 14. Fixtures and seed data

1. Prefer builders over golden files. A test-support package named `<domain>test` (for example `identitytest`, `repositorytest`, `gittest`, `containertest`, `webhooktest`) exposes `NewX(t, options...)` with valid defaults and named options. Builders return an unsaved value or, where persistence is involved, a value persisted in the caller's schema.
2. Test-support packages contain no production behaviour. A `go list`-based test asserts that no non-test package imports a package whose name ends in `test`.
3. Data-shaped fixtures (workflow YAML, devcontainer JSON, SAML XML, SCIM payloads, registry manifests, CODEOWNERS files) live in `testdata/` beside the package, each with a malformed counterpart.
4. Golden files are justified only when the artefact is itself the contract:
   - exact REST response JSON per public resource,
   - Markdown renderer output and the sanitizer allowlist,
   - generated release notes for a fixed commit graph,
   - the workflow JSON Schema,
   - the generated OpenAPI document,
   - error bodies for each documented failure class,
   - runner-protocol request and response fixtures,
   - webhook event payload schemas.
5. Golden-file rules: LF endings; no absolute paths; timestamps and identifiers normalized by a documented normalizer; updates only through an explicit `-update` flag with the resulting diff reviewed; a mismatch prints a unified diff; a golden file may never contain a real secret, token, credential, or personal data.
6. Fixture repository content is synthetic. Authors and addresses use reserved domains (`@example.invalid`). No production data, no real credentials, and no third-party code enters a fixture.
7. No test downloads a fixture at run time. Fixtures are committed or generated deterministically from a seed.
8. End-to-end seed data is created through the public API wherever the API can produce the state, so the fixture also exercises the API. Direct SQL seeds are allowed only for states the API cannot produce (a live session, an expired invitation, a lease) and each carries a comment naming why.
9. Time-dependent fixtures receive an injected clock. A fixture that encodes "now" is invalid.
10. Large binary fixtures (pack files, container images) are generated by a script, never committed, and the generator is byte-deterministic.

## 15. Concurrency and race-testing obligations

1. `go test -race ./...` with `GITNEST_TEST_DATABASE_URL` set is mandatory for the T2, T3, T4, T5, and T8 tiers.
2. Every workstream whose acceptance criteria mention concurrency adds a test that fails when the mechanism enforcing it is removed (unique constraint, row lock, advisory lock, lease, compare-and-swap, idempotency key). The test names the mechanism in a comment, and the reviewer removes the mechanism locally once to confirm the test fails. This review step is recorded in the change description.
3. Required concurrency cases:

| Area | Scenario | Invariant |
| --- | --- | --- |
| Invitations | N concurrent acceptances of one invitation | Exactly one succeeds |
| Issue and pull-request numbering | Concurrent creates in one repository | Numbers are unique and contiguous; no gap from a failed transaction |
| Repository create, rename, delete | Concurrent operations across two processes | One repository, one storage directory, no orphan; the loser gets a busy or conflict error |
| Merge against a push | Merge raced with a push to the base ref | The merge loses or refuses; the pushed commit is intact; no partial merge is recorded |
| Push to one ref | Two pushes raced | One winner; `git fsck` clean |
| Job claiming | Two workers claim from one queue | One execution per job |
| Lease expiry | A worker dies mid-job | The job is retried exactly once after the visibility timeout; logs already acked are kept |
| Concurrency groups | Three runs in one group | One running, one pending, the superseded run cancelled where configured |
| Artifact, cache, and package quotas | Concurrent uploads near a cap | Reported usage never exceeds the cap |
| Registration tokens | Reuse of a registration token | Refused |
| Rate limits | Concurrent requests across transports with one credential | One shared bucket; the counter survives a restart |
| Migrations | Two concurrent migration runners | Each migration applied once |
| Notification and webhook fan-out | The same event delivered twice | One user-visible notification; one delivery |
| Stars, reactions, votes | Concurrent toggles | Idempotent under a unique constraint; counts derived once |
| Workspace start | Two concurrent start requests | Exactly one container |
| Workspace admission | Concurrent admits at capacity | Instance capacity is never oversubscribed |
| Search reindex | Concurrent reindex of one repository | Identical final result |
| Audit log | Concurrent covered mutations | Exactly one record per mutation |

4. Test shape: a `parallel(t, n, fn)` helper with a start barrier so the goroutines contend, `errgroup`-style error collection, and assertions on the final persisted state, never on interleaving. Every concurrency test runs under a context deadline and sets PostgreSQL `lock_timeout` and `statement_timeout` low enough that a deadlock fails the test instead of hanging CI (5 s and 30 s by default in tests).
5. A concurrency test may not assert on wall-clock ordering. It asserts a database invariant, a constraint outcome, or a terminal state.

## 16. Time, waits, and flakiness

1. No `time.Sleep` in a test body. Waiting goes through a bounded polling helper (`eventually`) or a channel. The helper fails with the last observed state so a failure is diagnosable.
2. Default bounded wait: 2 s. Maximum: 10 s. Only `GITNEST_TEST_WAIT_SCALE` (1 to 4) may extend a deadline on a slow machine.
3. Domain code takes an injected clock. Only `internal/app` and `cmd/*` bind the real clock. A source-check test asserts that no domain package calls `time.Now` or `time.Sleep` directly.
4. Timers, tickers, retry schedules, rate-limit windows, idle reaping, session expiry, and lease expiry are driven by the injected clock and scheduler. A test advances the clock and runs the due-work pass instead of waiting.
5. Only the load tier asserts a duration, and only with a documented tolerance against a recorded baseline.
6. Flake policy: a test observed failing without a code change twice is quarantined with `t.Skip("flake: <tracker reference>")`, appears in the printed quarantine list, and must be fixed or deleted before the next release. Automatic retries are permitted only for the T5 and T7 tiers, at most once, and every retry is counted and reported; a retried pass is treated as a flake.
7. Every test that starts a process, server, or container registers cleanup with `t.Cleanup` and has a context deadline. `go test -timeout` stays at the default; each CI job sets a tighter timeout so a hang fails rather than occupying a runner.
8. Ports are bound as `:0` and the actual address is read back. Temporary roots come from `t.TempDir()`. No fixed port and no shared path.
9. Test order independence is a review requirement: a test that passes only when another ran first is a defect, not a flake.

## 17. Contract tests

### 17.1 Runner protocol

The contract fixtures live in `internal/runner/contracttest` and are consumed from both sides: the control plane is tested against a protocol-client double, and `cmd/gitnest-runner` is tested against a protocol-server double. One loopback test runs the real agent against the real server.

| Verb | Contract assertions |
| --- | --- |
| Register | Version header required; an unknown major is refused at registration with a distinguishable error; a runner below the supported minimum is refused; the registration token is single-use, expiring, and scoped |
| Poll and lease | A job with an unmatched label stays queued and is never leased by a mismatched runner; a lease token is scoped to the job and run; an expired lease token is refused |
| Heartbeat | A missed heartbeat expires the lease; the job returns to the queue exactly once under its idempotency key; acked logs are not lost |
| Status report | Terminal states are recorded once; a duplicate report is idempotent |
| Log chunk | Resumable from the last acked offset; the same chunk id twice is idempotent; a gap is rejected with the documented code; the stored log is masked |
| Artifact verbs | Upload, download, and list are scoped to the run token; a cross-repository read is refused |
| Cache verbs | Exact key and prefix restore; a cross-repository read is refused; the size cap is enforced before the blob is committed |
| Unknown verb | Refused with a machine-readable code, never a generic error |
| Compatibility | Within a major version, additions are optional; the client ignores unknown response fields (asserted with an extra-field fixture); every fixture change requires a version decision recorded in the runner protocol design note under `docs/design/` |

The fixture directory has a checked-in manifest, and a test fails when a verb's fixtures change without the manifest and the version decision changing with them.

### 17.2 Registry protocols

`internal/packages/contracttest` holds per-registry suites asserting request shapes, status codes, error bodies, content types, and the authentication challenge, independently of any native client.

| Registry | Required contract assertions |
| --- | --- |
| OCI / Docker | `/v2/` version check; `WWW-Authenticate` challenge shape; monolithic and chunked upload; blob existence check; manifest media-type validation; a manifest referencing an absent blob is rejected with the documented status and body; tag listing and pagination; catalog scoped to readable packages; referrers; digest format validation; deletion rules |
| npm | Packument full and abbreviated field parity; dist-tags; publish conflict rules; tarball integrity; unpublish rules; scope-to-owner mapping |
| Maven / Gradle | Layout paths; checksum files; snapshot uniqueness; metadata XML shape |
| NuGet | Service index; flat container; case-insensitive lookup; duplicate push status; symbol package |
| RubyGems | Compact index ordering; yank visibility rule; deterministic gzip of the specs index |
| Composer, Helm, generic | Metadata shapes; byte-preserving round trip; cross-kind name isolation |

Every suite includes the unauthorized case and asserts the documented denial shape with no existence disclosure. `Registry` and `BlobStore` are also contract-tested as interfaces: no HTTP type appears in a signature, and the local and S3-compatible `BlobStore` implementations pass one shared suite.

## 18. Boundary to required test kinds

| Boundary | Required test kinds |
| --- | --- |
| Credentials, sessions, tokens, second factors | T1 unit (hashing, constant-time comparison, TOTP window, WebAuthn counter) and T2 concurrency (single-use invitation, revocation) plus T6 for the browser flow |
| Shared authorization service | T1 decision table over every principal and action; T2 grant persistence; a cross-transport test asserting REST, GraphQL, HTTPS Git, SSH, and registry return the same decision |
| Repository storage and the Git command runner | T2 bare-repository lifecycle, traversal refusal, crash repair; T3 real-client operations, cancellation, `git fsck` |
| Git transports | T3 conformance; T1 route parsing; log-capture assertions that no token, key, or pack byte is logged |
| Markdown, diff, and archive rendering | T1 hostile-input corpus; T2 golden artefacts; T7 time and memory under cap |
| PostgreSQL persistence | T2 per-schema tests; uniqueness and lock tests; index-plan tests required by [performance.md](performance.md) |
| Queue, outbox, and leases | T1 schedule and idempotency with a fake clock; T2 claim, lease, retry, dead-letter; T3 and T4 crash recovery |
| Workflow engine | T1 parser, expression, and matrix fixtures; T2 run state machine; T4 isolation, secrets, containers, cleanup |
| Runner protocol | T1 protocol fixtures; T2 lease persistence; T4 a real job; T3 the real agent over loopback |
| Container runtime adapter | T4 contract suite for both the job and workspace callers; T1 fake-runtime tests for the no-runtime path |
| Packages and releases | T1 metadata and retention rules; T5 native-client conformance; T2 quota concurrency; a restore drill |
| Webhooks and mail | T1 signing, backoff, and SSRF decisions with an injected clock; T2 outbox exactly-once and the local sink suite |
| Search | T1 qualifier parser; T2 permission-filtered results; a SQL-against-index conformance comparison on a fixture corpus; T7 latency |
| SSO and SCIM | T1 assertion validation fixtures; T2 stub IdP and SCIM driver; nightly real-IdP interop |
| Web UI | T1W component tests; T6 browser flows, keyboard paths, escaping, and performance budgets |
| API contract | T1 response-struct allowlist and goldens; T2 error, pagination, ETag, and rate-limit tables; T6 executed documentation examples |
| Deployment and lifecycle | T2 migrate, start, readiness, shutdown; T6 and T7 upgrade and zero-downtime rehearsal |

## 19. Review checklist for a test change

1. Does the test name its tier and gate?
2. Is there a denied case for every allowed case where permission is involved?
3. Is the concurrency invariant asserted, and would the test fail without its lock or constraint?
4. Are both sides of every affected limit covered: just below, and at or over?
5. Does the test avoid sleeps, fixed ports, shared paths, and ambient environment variables?
6. Are fixtures built by builders, with golden files only where §14.4 allows them?
7. Are logs captured and asserted free of secrets, tokens, and repository content?
8. Is cleanup registered for every process, container, schema, and temporary directory?
