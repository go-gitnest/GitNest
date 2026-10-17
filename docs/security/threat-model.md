# GitNest threat model

**Status:** Target design. This document specifies the threats GitNest must withstand and the mitigations each surface must carry. It describes required behaviour, not implemented behaviour. No claim in this document is evidence that a control exists.

**Scope:** The attack surfaces of the self-hosted GitNest instance: Git transports and the Git command runner, repository content rendering, archive and LFS generation, uploads, the REST and GraphQL APIs, webhooks, package registries, Actions job execution, cloud-development workspaces, Pages, authentication, and resource exhaustion across all of them.

**Out of scope by product decision:** the mobile application, GitHub's own Codespaces, Advanced Security (code scanning/CodeQL, secret scanning, Dependabot), Gist, payments and Sponsors, Copilot and AI features, and Marketplace payments. These are decided non-goals in [product-goals.md](../product-goals.md) and have no threat surface to model here.

## 1. Assumptions

| # | Assumption | Consequence if it fails |
| --- | --- | --- |
| A1 | The reverse proxy terminates TLS and forwards HTTP. GitNest authenticates and authorizes; the proxy is not an authorization boundary. | A proxy-only or header-based trust decision becomes remotely bypassable. |
| A2 | Public URLs are built from the configured `GITNEST_PUBLIC_URL`. Request `Host`, `X-Forwarded-Host`, and `X-Forwarded-Proto` are never inputs to URL generation, redirects, WebAuthn origins, or OAuth callback matching. | Host-header injection, open redirect, and origin widening. |
| A3 | Authorization is decided in shared application services. Browser, REST, GraphQL, HTTPS Git, SSH Git, registries, runners, and workspaces all call the same decision function and pass a principal, never a scope field. | A transport forks the permission model and a denial path is missed. |
| A4 | The web process never executes repository-controlled code: no workflow step, no devcontainer command, no repository hook, no package install script. | A single crafted repository yields remote code execution in the process that holds database credentials. |
| A5 | Untrusted execution (Actions jobs, workspaces, Pages builds) runs outside the web process and never holds a credential that reaches GitNest's database, its secret key, the container-runtime socket, or another tenant's storage. | Full instance compromise from one workflow or one workspace. |
| A6 | Repository and storage paths derive from internal IDs. A client never supplies a filesystem path, and no request value is interpolated into a shell command. | Path traversal and command injection in the Git runner. |
| A7 | The single-node baseline is the supported default: one binary, PostgreSQL, one repository storage root. Object storage, a search service, a broker, extra nodes, and a container runtime are optional. | A mitigation that depends on optional infrastructure silently disappears on the baseline install. |
| A8 | Secrets (passwords, session and invitation tokens, personal access tokens, private keys, webhook secrets, action secrets, registry credentials) are never written to logs, error responses, metric labels, audit detail fields, or queue payloads. | Secret disclosure through the observability surface, which is retained and widely readable. |

## 2. Shared design rules that every surface relies on

| Ref | Rule | Enforcement point |
| --- | --- | --- |
| R1 | One authorization decision function for every principal kind (user, anonymous, personal access token, fine-grained token, OAuth token, app installation token, deploy key, workflow token, runner, workspace). | `internal/<domain>` application services |
| R2 | A denial that would disclose the existence of a private resource returns the same not-found response used elsewhere. | Domain services; transports map the result |
| R3 | Every limit is enforced at the shared service or platform adapter, not in the UI and not only in a transport. | Domain services and `internal/platform/*` |
| R4 | Rate limits are keyed by credential when one is presented, and shared across every transport that authenticates the same credential. | Rate-limit service (`internal/identity`, extended in phase 4) |
| R5 | Consequential actions emit an audit event in the same transaction as the change. A failed audit write fails the operation. | Domain services and `internal/activity` |
| R6 | Untrusted output is data: escaped for its destination, bounded in size, and never re-parsed as a command, path, or configuration value. | Renderers, log pipeline, workspace control plane |
| R7 | Integration tests that touch PostgreSQL, Git, storage, or a container runtime are gated by `GITNEST_TEST_DATABASE_URL` and use a schema-per-test helper (built in W0.9) rather than a database-per-test helper. | Test harness |

Test obligation names in this document are the tests that must exist. They are identified by package and case so the allowed and denied cases are both explicit.

## 3. Surface (a): Git smart HTTP and SSH transports, and the Git command runner

**Assets at risk:** repository objects and refs; private repository content and existence; repository storage root integrity; the service account that runs Git; Git credentials presented by clients; per-operation temporary data.

**Attacker capabilities assumed:** an unauthenticated network client that can send arbitrary smart-HTTP requests and protocol-v2 commands; an authenticated user with read on a public repository and no access to a private one; a user with a valid token for repository A attacking repository B; an SSH client presenting an arbitrary key, username, or environment request; control of repository content that the server will process (large objects, hostile pack shapes, crafted ref names).

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-A1 | Command injection: request data (service name, repository path, ref, remote user, `-o` options) reaching a shell or a Git argument. | Argument arrays only; no shell; a fixed allowlist of services (`upload-pack`, `receive-pack`, `upload-archive`, LFS verbs); every client-supplied value validated against a strict grammar before becoming an argument. | `internal/platform/git`, `internal/gittransport` |
| T-A2 | Path traversal: `../`, absolute paths, symlinks, or alternate repository URLs resolving outside the storage root. | Paths resolve from internal IDs and a canonical joining routine; the resolved absolute path is verified inside the repository root after symlink evaluation; repository names reject separators, control characters, and reserved names. | `internal/platform/storage`, `internal/repository` |
| T-A3 | Authorization bypass across transports: SSH accepts a push that HTTPS denies, or the transport decides access itself. | Both transports call the same `gittransport` authorization for read and write; the principal carries no pre-decided permission; a transport that answers an access question is a defect. | `internal/gittransport` |
| T-A4 | Existence disclosure: recognizable responses for a private repository's clone, archive, or LFS batch. | Anonymous and denied access return the same not-found shape as a nonexistent repository; timing and error text do not differ by existence. | `internal/gittransport`, `internal/transport/http/git` |
| T-A5 | Server-side hook execution from repository content, or an unexpected Git service request. | No repository-provided hook is ever executed; hooks are not read from the repository; the runner starts Git with a controlled environment and refuses any service outside the allowlist. | `internal/platform/git` |
| T-A6 | Credential or configuration exposure through the Git child process environment. | Minimal explicit environment; inherited variables that carry credentials, proxy settings, `GIT_ASKPASS`, `SSH_ASKPASS`, or cloud metadata endpoints are cleared; Git configuration is passed explicitly, not read from a writable home. | `internal/platform/git` |
| T-A7 | Ref-update races: a concurrent push or merge silently overwriting a newer ref. | Ref updates use compare-and-swap semantics; repository-scoped advisory locks or per-ref expected-value checks; a failed precondition returns a distinct busy/conflict error. | `internal/platform/git`, `internal/pullrequest` |
| T-A8 | Resource exhaustion inside one Git operation: unbounded pack size, object count, or process lifetime; a process that outlives its request. | Bounded wall-clock and idle timeouts, bounded request and response bytes, a process-group kill on timeout, cancellation propagated from the client connection, and temporary data removed on success, failure, cancellation, and process crash. | `internal/platform/git` |
| T-A9 | SSH surface abuse: PTY allocation, port or agent forwarding, exec of arbitrary commands, username enumeration, or a key used against a repository it does not own. | Key-only path with no shell, no PTY, no forwarding; unknown usernames and unmatched keys produce the same failure; deploy keys are bound to exactly one repository and are read-only or read-write by explicit setting; revocation takes effect on the next connection. | `internal/transport/ssh` |
| T-A10 | Credential material leaking into logs via clone URLs or headers. | Credentials in URLs, headers, and query parameters are redacted before any log line; no request data is logged at a level that includes secret fields. | `internal/platform/logging` |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: token with read on a private repository | `internal/gittransport` integration test authenticates and streams `upload-pack`; response contains the requested refs. |
| Allowed: SSH key registered to the account, no PTY requested | `internal/transport/ssh` test authenticates, resolves the repository by key, and dispatches `git-upload-pack`. |
| Denied: `service=shell`, `service=..%2f..`, or an unknown service | Request rejected before any process starts; Git invocation counter stays at zero. |
| Denied: traversal repository name and a symlinked repository directory | `internal/platform/storage` test asserts resolution fails and nothing outside the root is read or written. |
| Denied: token for repository A against repository B | `internal/gittransport` test returns the not-found response and starts no process. |
| Denied: SSH request for PTY, forwarding, or an exec command | Connection refused with the same error as an invalid key; no process starts. |
| Denied: push to a protected ref without the required grant | Receive rejected, ref unchanged, audit event written. |
| Abort: client disconnects mid-clone; operation exceeds its timeout | Process group is reaped within the documented bound, no temporary directory remains, and the timeout is distinguishable in the outcome metric. |
| Concurrency: two pushes to the same ref | Exactly one succeeds; the other receives a conflict error; ref history shows no interleaving. |

## 4. Surface (b): repository content rendering — Markdown, diffs, syntax highlighting, raw file view

**Assets at risk:** the browser session of every viewer (session cookie, CSRF token, page content of other repositories); private repository content; the rendered-page cache if a shared cache is configured.

**Attacker capabilities assumed:** a repository contributor who controls file content, file names, Markdown bodies, commit messages, branch and tag names, and symlink targets committed into the repository; an unauthenticated viewer of a public repository.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-B1 | Stored XSS: raw HTML, inline event handlers, `javascript:` / `data:` URLs, or SVG script content in Markdown or a rendered file. | Markdown rendering with raw HTML disabled, a sanitizer as the final step, a link-protocol allowlist (`http`, `https`, `mailto`), and no inline event attributes; rendered output is treated as untrusted even after sanitizing. | Markdown renderer (phase 2 W2.1), `internal/transport/http/web` |
| T-B2 | XSS through attribute and CSS contexts: user text in `title`, `href`, `class`, or a color value. | Attribute values are quoted and encoded by a contextual encoder; user-controlled colors and identifiers are validated against a grammar, never interpolated raw. | Renderer and React components |
| T-B3 | Directory traversal through file paths and symlinks in the tree or raw view, including a symlink pointing outside the repository. | Paths are resolved inside the repository tree for the requested commit; symlinks are shown as their target text and never followed outside the tree; the raw view serves blob content only. | `internal/repository`, `internal/platform/git` |
| T-B4 | Diff rendering attacks: terminal escape sequences, control characters, bidirectionally-overriding characters, or extremely long lines. | Control and format characters are stripped or replaced with visible placeholders in diff and file views; line length is bounded with an explicit truncation marker; binary content is summarized, not rendered. | Diff renderer (phase 3 W3.2, W3.3) |
| T-B5 | Cross-viewer leakage through a shared rendered-content cache. | A cached rendered payload is keyed by a value that already accounts for the viewer's permission; cached output is never keyed by content alone when it embeds a permission-dependent URL. | `internal/platform/cache` (phase 10 W10.6) |
| T-B6 | Cross-repository or cross-origin resource embedding: remote images or iframes that leak viewer IP, referrer, or existence of a private path. | External resource embedding is restricted to a documented allowlist of image hosts or proxied through GitNest; referrer policy and content-security policy are set instance-wide. | `internal/transport/http/web` |
| T-B7 | Template injection through commit messages, branch names, or issue titles reaching a template or a shell. | Templates are server-side with escaped interpolation; no user value is evaluated as a template, a path, or a command. | Renderer, `internal/transport/http/web` |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: Markdown with fenced code, links, tables, task lists, and emoji shortcodes | Renders with the expected elements; links keep only allowlisted protocols. |
| Allowed: raw view of a text blob at a commit | Bytes match the blob exactly; content type is not sniffed into an executable type. |
| Denied: `<script>`, `<img onerror=...>`, `javascript:` link, `data:text/html` link, SVG with script | Rendered output contains no executable content and no disallowed protocol; the sanitizer test asserts the element or attribute is absent. |
| Denied: BOM/RTLO and control characters in a file name, commit message, and diff line | Output contains a visible placeholder; no raw control character reaches the HTML. |
| Denied: symlink to `/etc/passwd` and a `../../` tree path | Request returns not-found; no file outside the repository root is opened. |
| Denied: two viewers with different permissions requesting the same rendered URL | The cached entry is not shared across the permission boundary; the viewer without access receives not-found. |
| Denied: 10 MB single-line diff and a 100 000-file comparison | Response is truncated with an explicit marker within the documented byte cap, and the request completes. |

## 5. Surface (c): archive and LFS generation

**Assets at risk:** CPU, memory, bandwidth, and temporary disk on the web node; LFS objects belonging to other repositories; repository existence.

**Attacker capabilities assumed:** an unauthenticated client requesting archives of public repositories; an authenticated client with read on one repository requesting LFS objects from a private repository; control of repository content including pointer files and object sizes.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-C1 | Archive bombs and unbounded generation: a large or highly compressible tree consuming CPU, memory, or disk. | Archive generation streams with a bounded total input size, a bounded entry count, a wall-clock timeout, and a maximum compressed and uncompressed byte cap; the request is abandoned and temporary data removed when a cap is hit. | `internal/gittransport`, `internal/platform/git` |
| T-C2 | Content-Disposition and filename injection from ref or repository names. | Archive file names derive from validated identifiers; the header value is encoded per RFC 6266 with both a plain and an encoded form. | `internal/transport/http/git` |
| T-C3 | LFS enumeration and cross-repository object access by guessing an object ID. | Every LFS batch and object request is authorized through the repository service for the repository in the batch; objects are addressed by verified content hash and served only for a repository the principal can read. | `internal/gittransport`, `internal/platform/storage` |
| T-C4 | LFS pointer or object integrity abuse: a client claiming an object the digest does not match, or uploading an object with a mismatched size. | The declared size and digest are verified against the received bytes before the object is finalized; a mismatch rejects the object and leaves no entry behind. | `internal/gittransport`, `internal/platform/storage` |
| T-C5 | Temporary archive or LFS staging data escaping cleanup and filling the disk. | Every temporary path is created under a per-operation root that is removed on success, failure, cancellation, and process restart; a startup sweep removes stale roots. | `internal/platform/storage` |
| T-C6 | Resource exhaustion by many concurrent archive requests. | Per-credential and per-source-address concurrency and rate limits on archive and LFS endpoints, with a documented budget shared across transports. | Rate-limit service |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: archive of a public repository tag | Streams a valid tar.gz; content matches a local checkout. |
| Allowed: LFS batch for a readable repository with an existing object | Returns a download action with an expiring URL; the object downloads and its digest matches. |
| Denied: LFS batch and object download for a repository the principal cannot read | Not-found response; object store is not read; no action URL is issued. |
| Denied: object upload whose bytes do not match the declared digest or size | Upload rejected, no object finalized, digest mismatch recorded. |
| Denied: archive request exceeding entry count, byte cap, or timeout | Request terminates with a clear error within the bound; temporary directory is gone. |
| Denied: ref name crafted to inject a header value | `Content-Disposition` contains no CR, LF, or unescaped quote. |

## 6. Surface (d): file uploads and attachments

**Assets at risk:** the storage root and its permissions; other tenants' uploads; the operator's disk quota; the browser of a user who later views the upload; server memory during decoding.

**Attacker capabilities assumed:** an authenticated user uploading arbitrary bytes, declaring an arbitrary content type and file name, uploading many files, and uploading a file crafted to exploit a parser; a user who can post the resulting URL in an issue or comment.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-D1 | Path traversal or file overwrite through a supplied file name. | Stored names are generated from internal IDs and the verified content digest; the client name is kept only as display metadata and is validated for control characters and length. | `internal/platform/storage` |
| T-D2 | Decompression or decode bomb in an uploaded image or archive processed for a thumbnail or preview. | Uploads are size-capped before storage; any decoder runs with a bounded pixel count or expansion ratio, a time limit, and an output cap; failure discards the temp file. | `internal/platform/storage`, decoder call sites |
| T-D3 | Content-type confusion: an executable or HTML payload served inline as an image or document. | Serving decisions come from the stored validated type (or a forced download disposition), never from the client-declared type; `Content-Type` is explicit and `X-Content-Type-Options: nosniff` is set. | Upload serving path |
| T-D4 | Quota and disk exhaustion by many or large uploads. | Per-user, per-organization, and per-instance byte and count quotas are checked under a transaction lock before the write is committed; the upload path checks free disk before accepting. | Quota service, `internal/platform/storage` |
| T-D5 | Serving user-uploaded SVG or HTML from the application origin, enabling same-origin script execution. | Active content types are served from a separate origin, forced to download, or sanitized; an upload never renders as a document on the session origin. | Upload serving path |
| T-D6 | Upload retained without an owner afterwards, or reachable by guessing a URL. | Every upload row references an owner and a target; access follows the target's permission; deleted targets delete their uploads; identifiers are unguessable and authorization is rechecked on read. | `internal/repository`, `internal/issues`, `internal/pullrequest` |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: PNG and PDF upload to an issue, then download by a user with read | Downloads with the stored content type and correct bytes. |
| Allowed: upload by a user with write, deleted by the owner | Row and blob are removed; subsequent read returns not-found. |
| Denied: file named `../../etc/cron.d/x`, with NUL bytes, or with a 4 000-character name | Stored under a generated name; the original is rejected or stored as inert display metadata; nothing outside the upload root is created. |
| Denied: 100 MP decompression bomb and a zip with a 10 000:1 ratio | Decoder aborts within the bound, temp file removed, upload rejected. |
| Denied: SVG with script uploaded and requested from the session origin | Response cannot execute script on the GitNest origin (download disposition or isolated origin). |
| Denied: upload exceeding the user quota, and upload when free disk is below the reserve | Request rejected before the write is committed; no partial blob remains. |
| Denied: download of another tenant's upload by URL | Not-found; the object store is not read. |

## 7. Surface (e): REST and GraphQL APIs, token and OAuth/app credentials

**Assets at risk:** all domain data reachable through the API; personal access tokens, OAuth tokens, app private keys, and installation tokens; the rate-limit budget; the audit trail's integrity.

**Attacker capabilities assumed:** an unauthenticated caller with an arbitrary credential string; a caller with a token of one scope or repository selection attempting a wider operation; a malicious OAuth client or registered app; a caller able to replay a captured request; a client that can issue deeply nested GraphQL queries or expensive filters.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-E1 | Authentication failures that leak credential validity or allow offline guessing. | Generic authentication errors; hashed storage of every token and secret with a short display prefix; constant-time comparison; uniform timing and response shape for unknown, revoked, and expired credentials. | `internal/identity`, `internal/authapp` |
| T-E2 | Scope or permission escalation: a read-scoped token, a single-repository fine-grained token, or an app installation token acting outside its grant. | One decision function composes user grants, classic scopes, fine-grained permission sets, app installation permissions, organization policy ceilings, and visibility; a transport that inspects a scope field is a defect. | `internal/authapp` / `internal/repository` decision function (phase 4 W4.11) |
| T-E3 | Object reference substitution: incrementing an internal ID or swapping a repository ID to reach another tenant's resource. | Every request resolves the resource through its owning service under the principal; identifiers are not authorization; cross-tenant reads return not-found. | Domain services |
| T-E4 | Over-posting and mass assignment: a request body setting fields the caller may not set (`is_admin`, `owner`, `visibility`, `permissions`, storage identifiers, timestamps). | Explicit request structs per endpoint with an explicit allowlist of settable fields; no direct row binding from JSON. | `internal/transport/http/api` |
| T-E5 | Response leakage: credential fields, hashes, internal IDs not intended for the surface, or private-resource existence in totals and error text. | Explicit response structs; a shared error mapper that emits no SQL text, stack trace, or internal identifier; list filtering before pagination counts so private items cannot shift a public total. | `internal/transport/http/api` |
| T-E6 | Authorization bypass through GraphQL: a resolver reading rows directly, an alias bypassing a depth limit, an introspection-driven surface enumeration, or batched queries multiplying a permission check. | Resolvers call the same application services as REST; query depth and complexity limits are rejected before execution; field-level authorization on every resolver; introspection disabled by default in production. | `internal/transport/http/api` (phase 4 W4.7) |
| T-E7 | OAuth and app credential abuse: redirect-URI manipulation or open redirect, missing or reusable `state`, PKCE omission, code interception, a long-lived or over-scoped installation token, or a leaked app private key. | Exact redirect-URI match against registered callbacks; single-use `state`; PKCE for public clients; short-lived authorization codes bound to client, redirect URI, and verifier; installation tokens expire within one hour and are individually revocable; private keys encrypted at rest with the instance key and excluded from logs, responses, and audit detail. | `internal/authapp` (phase 4 W4.9, W4.10) |
| T-E8 | Credential replay and non-revocation: a captured request replayed, or a revoked token still accepted because a transport or cache keeps its own view. | One revocation path used by UI, API, and admin views; revocation takes effect on the next request in every transport; no transport-local credential cache; TLS is the only replay protection for bearer credentials, so tokens carry expiry and last-used recording. | `internal/identity`, rate-limit and session services |
| T-E9 | Rate-limit evasion and abuse: rotating source addresses to multiply a token's budget, unbounded expensive endpoints (search, diffs, archives, GraphQL complexity), or enumeration by timing. | Buckets keyed by credential when one is presented and by source address otherwise, shared across transports; separate budgets for authenticated, unauthenticated, and app-installation callers; expensive endpoints have their own limits and a `Retry-After` on rejection. | Rate-limit service (phase 4 W4.4) |
| T-E10 | Audit gaps: a credential created, widened, or revoked without a record. | Every credential creation, scope change, installation, and revocation is audited with actor, credential kind, and scope, never with the secret value. | `internal/activity` |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: classic token with `repo` reading and writing a repository the user can access | Requests succeed; `X-RateLimit-*` headers are present and consistent. |
| Allowed: fine-grained token selecting one repository | Succeeds on the selected repository; unaffected endpoints degrade normally. |
| Allowed: app installation token on a selected repository within its permissions | Succeeds; token expiry is enforced. |
| Denied: fine-grained token selecting repository A against repository B | Not-found response identical to a nonexistent repository. |
| Denied: read-scoped token on a write endpoint; app token on an endpoint outside its permission set | Forbidden or not-found per the disclosure rule; no state change. |
| Denied: request body setting `is_admin`, `owner`, or `visibility` on an endpoint that does not accept it | Field ignored or rejected with a `422`; stored value unchanged. |
| Denied: revoked token, expired token, and a token after password change | All rejected on the next request in API, Git HTTP, and Git SSH paths. |
| Denied: GraphQL query beyond depth or complexity, and a resolver for a private object | Rejected before execution with a clear error; introspection disabled in production configuration. |
| Denied: OAuth flow with a mismatched redirect URI, a reused `state`, or a missing PKCE verifier | Flow rejected at the authorization or token step; no token issued. |
| Denied: 200 requests with a valid token from 50 source addresses | One shared bucket is consumed; the limit is reached at the documented count. |
| Denied: two paginated list requests, one from a user who can see a private item | Totals and `Link` headers are computed after permission filtering and do not reveal the item. |

## 8. Surface (f): webhooks — outbound delivery, SSRF via target URLs, signature replay

**Assets at risk:** the internal network and cloud metadata endpoints reachable from the web process; webhook signing secrets; the delivery log; webhook consumers' trust in the payload.

**Attacker capabilities assumed:** an authenticated user who can configure a webhook target URL and choose the secret; a third party who can observe a delivery (for example a compromised or hostile receiver) and replay it; a hostile DNS server or an HTTP redirect chain controlled by the target; a target that responds slowly or with a huge body.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-F1 | SSRF: a target URL pointing at loopback, link-local, private ranges, the cloud metadata address, or the instance itself. | Destination validation on every attempt: resolve the hostname, reject non-public addresses unless the operator has explicitly allowlisted them, connect to the resolved address, and re-check after every redirect; redirects are disabled unless configured otherwise. | `internal/webhooks` delivery worker |
| T-F2 | DNS rebinding between validation and connection. | Resolve, validate, and connect using the same validated address (pinned resolution), with no second resolution by the HTTP client. | `internal/webhooks` delivery worker |
| T-F3 | Signature forgery or a digest computed over different bytes than those transmitted. | `X-Hub-Signature-256` HMAC-SHA256 over the exact transmitted bytes; constant-time comparison; the payload is serialized once and both transmitted and signed from that buffer. | `internal/webhooks` (phase 4 W4.12) |
| T-F4 | Replay of a valid delivery to a target that treats it as fresh. | A unique delivery GUID and a timestamp in the headers, documented as the receiver's replay defence; GitNest never reuses a delivery GUID for a distinct payload; manual redelivery is recorded as its own attempt. | `internal/webhooks` (phase 4 W4.12, W4.13) |
| T-F5 | Slow or oversized responses consuming worker capacity, or a response body used as an injection vector. | Bounded connect and total timeouts, no retry on `4xx`, bounded backoff and attempt count, a capped response-body prefix stored in the delivery log, and the stored prefix treated as untrusted text in the UI. | `internal/webhooks` delivery worker |
| T-F6 | Secret disclosure: the signing secret or a payload secret appearing in the delivery log, an error, or an audit event. | The delivery record stores headers and a body digest, never the secret header value; payloads that contain credentials are built by the owning domain, which excludes secret material. | `internal/webhooks`, `internal/activity` |
| T-F7 | Retry storms and duplicate delivery to a struggling consumer. | Retries are durable, bounded with exponential backoff and jitter, scoped per subscription, and idempotent from the consumer's view; a subscription can be disabled by an operator or by repeated terminal failures. | `internal/webhooks`, `internal/jobs` |
| T-F8 | Delivery to a target that is itself a GitNest endpoint, amplifying load. | Self-target detection against the configured public URL and the instance's own addresses, rejected unless the operator explicitly allows it. | `internal/webhooks` delivery worker |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: delivery to a test HTTP server on a public fixture address | Signed payload arrives; the receiver verifies the HMAC over the raw body; the delivery log records status and duration. |
| Allowed: operator allowlists an internal address | Delivery to that address succeeds only with the explicit allowlist entry present. |
| Denied: target `http://127.0.0.1:8080/`, `http://169.254.169.254/`, `http://[::1]/`, and a private RFC1918 address | No connection is attempted to the target; the delivery is recorded as blocked with a distinguishable reason. |
| Denied: hostname that resolves to a public address first and a private address on re-resolution | The connection uses the validated address; the private address is never contacted. |
| Denied: redirect from a public URL to `http://169.254.169.254/` | Redirect not followed; delivery fails with the block reason. |
| Denied: payload tampered after signing, and a signature computed over re-serialized JSON | Verification fails; the receiver rejects. |
| Denied: two deliveries with the same body but different GUIDs, and one GUID reused | GUIDs differ per attempt; the delivery log shows distinct attempts. |
| Denied: target that never responds and a target returning a 100 MB body | Attempt times out within the bound; stored body prefix is capped; no retry on `4xx`. |

## 9. Surface (g): package registries — untrusted uploads, name squatting, blob integrity

**Assets at risk:** package blobs and their integrity; storage quota; the owner namespace; the digest of a published artifact that downstream consumers trust; the registry namespace that authentication and authorization map to.

**Attacker capabilities assumed:** an authenticated user with package-write on one owner publishing arbitrary bytes and metadata; a user attempting to publish into another owner's namespace or a name reserved for another package; a user crafting archive or index metadata that a client will parse; a client attempting to fetch a blob it cannot read.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-G1 | Path traversal or key collision from client-supplied names, versions, or paths. | Per-registry grammar validation for names and versions; storage keys derive from internal IDs and content digests; no client value is concatenated into a filesystem path. | `internal/packages` |
| T-G2 | Name squatting and namespace confusion: claiming a name reserved for an organization, a registry route, or an unowned scope; lookalike names used to mislead consumers. | Name normalization rules per registry (case sensitivity explicit); an owner namespace that a package must be published into; reserved names rejected at publication; a name is owned by exactly one owner and transfer is an audited operation. | `internal/packages`, `internal/organization` |
| T-G3 | Blob integrity: a stored blob whose bytes do not match the recorded digest, or a manifest referencing a missing blob. | Digest verification on upload before the version record is created; a scheduled scrub re-verifies and quarantines mismatches; a manifest referencing an absent blob is rejected; blobs are content-addressed and written to a temporary path then renamed. | `internal/packages` (phase 6 W6.4) |
| T-G4 | Malicious archive or metadata: a tarball with traversal entries, a zip bomb, a huge index, or a deeply nested metadata document. | Archives are never extracted outside a per-operation temp directory; expansion and entry counts are capped; metadata parsing is bounded in size and depth; no uploaded content is ever executed or installed by GitNest. | `internal/packages` |
| T-G5 | Unauthorized read or write: publishing into another owner, deleting another owner's version, or downloading a private package blob. | Every registry verb authenticates to the same principal and calls the same package decision function, extended from the phase 4 permission registry; visibility is inherited from the owning repository or organization with no separate switch. | `internal/packages` (phase 6 W6.11) |
| T-G6 | Quota bypass by concurrent uploads, and unbounded retention growth. | Byte-limit checks run under a transaction lock before the upload is finalized so two concurrent uploads cannot exceed the limit; retention rules run as a scheduled job with a dry-run report before deletion. | `internal/packages` (phase 6 W6.12) |
| T-G7 | Token or bearer leakage in registry URLs and error responses. | Registry credentials in URLs, `Authorization` headers, and `WWW-Authenticate` challenge echoes are redacted; error bodies contain no credential material. | `internal/packages`, `internal/platform/logging` |
| T-G8 | Range requests or redirected download URLs granting access to a package the caller cannot read. | A download URL is issued only after the read decision and is short-lived and bound to one blob; authorization is rechecked when the request is served by GitNest. | `internal/packages` |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: publish an npm package and an OCI image into the caller's own namespace, then pull them | Pull succeeds; the recorded digest and `shasum` match the retrieved bytes. |
| Allowed: publish a Maven artifact with checksums and consume it through the standard layout | Paths and checksums resolve as a Maven client expects. |
| Denied: publish `@other-org/pkg` and set a manifest config digest that does not match the uploaded blob | Rejected before a version record exists; no blob finalized. |
| Denied: upload a tarball with `../` entries and an archive with a 10 000:1 expansion ratio | Nothing is written outside the temp directory; the upload fails within the bound. |
| Denied: push to a package the caller can only read, and delete another owner's version | Forbidden; no version record changes; audit event absent for the denied attempt's target. |
| Denied: two concurrent uploads that together exceed the owner byte limit | Exactly one succeeds; the limit is never exceeded; the second receives a quota error. |
| Denied: download a private package blob without a token, and with a token for another owner | Rejected; no blob bytes served; the issued URL path is not usable. |
| Scrub: corrupt a stored blob on disk | The scheduled scrub detects the mismatch, quarantines the blob, and reports it without serving the corrupted content. |

## 10. Surface (h): CI job execution

**Assets at risk:** repository, organization, and environment secrets; the workflow token and any credential reachable from a job; the GitNest database and its signing key; other tenants' caches, artifacts, and logs; the runner host; the queue's integrity.

**Attacker capabilities assumed:** a contributor who can author a workflow in a repository they have write access to, including a fork pull request where the workflow runs with reduced privilege; a job that runs arbitrary code, network calls, and container images it selects; a repository that controls action references, cache keys, artifact names, and log content; a malicious or compromised self-hosted runner.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-H1 | Credential exfiltration from a job: database credentials, the instance secret key, the container-runtime socket, or the host repository root mounted into the job container. | The runner is a separate trust domain with no database credential; the control plane never hands a job a credential that reaches GitNest data; job containers never mount the host repository root, the runtime socket, or the configuration directory; the worker's database role is limited to run, job, log, lease, cache, and artifact tables and never to identity or session tables. | `internal/runner`, `internal/platform/container` (phase 5) |
| T-H2 | Secret leakage through logs, errors, metrics, artifacts, or the failure path of the executor itself. | Every resolved secret value and its base64 form are registered with a masker and rewritten before any storage or streaming path; masking is tested against base64, multi-line, JSON-escaped, and substring-overlapping values; the executor's own failure path is covered by the same rule. | `internal/actions` masker (phase 5 W5.4, W5.11) |
| T-H3 | Cache poisoning and cross-tenant cache reads: a cache entry written by one branch, repository, or tenant and restored by another. | Cache entries are repository-scoped and branch-scoped, keyed exactly, restored through prefix `restore-keys` only within that scope; a cache from a fork pull request cannot be written into the base repository's scope; per-repository and instance size caps apply with LRU eviction. | `internal/actions` (phase 5 W5.10) |
| T-H4 | Workflow-token privilege escalation: default write scopes, a permission widened without repository policy, a token outliving the run, or a token reaching another repository. | The workflow token is minted per run with a default read-only permission set, widened only by an explicit `permissions` entry the repository policy allows, scoped to the originating repository, and expiring at run end; the shared authorization service enforces declared permissions, not the transport. | `internal/actions` (phase 5 W5.14) |
| T-H5 | Fork pull-request abuse: secrets exfiltrated, a write token used to modify the base repository, or a workflow changed by the pull request to escalate. | Fork pull requests run with no secrets and a read-only token; the workflow definition used is the one at the triggering ref subject to repository policy; environment protection rules are evaluated by the shared service before secret injection, so no transport can bypass them. | `internal/actions` (phase 5 W5.14, W5.13) |
| T-H6 | Malicious action reference: an unpinned `uses` ref swapped after review, a Docker action from an untrusted registry, or a nested action chain hiding the real source. | `uses` references are pinned by full commit SHA and unpinned refs are rejected; every resolved reference is recorded on the run with its resolved SHA and source; container images are pulled from configured registries under a size and digest policy; nesting depth is bounded and recursion rejected. | `internal/actions` (phase 5 W5.7, W5.8) |
| T-H7 | Untrusted-input parsing: a hostile workflow YAML, expression string, artifact or cache name, job output, or log line exploiting the parser. | Strict YAML decoding that rejects duplicate keys, unknown keys, and unsupported syntax; a bounded workflow file size and job/step count; an expression evaluator with no filesystem, network, or process access beyond a bounded `hashFiles` inside the checkout; artifact, cache, and log names validated against a grammar; job outputs bounded in size and treated as text. | `internal/actions` (phase 5 W5.1, W5.4) |
| T-H8 | Cross-tenant job leakage: a job reading another run's logs, artifacts, or workspace, or leasing a job it does not own. | Every log, artifact, and cache read is authorized against the run and repository; the runner protocol binds a lease to a runner scope so a token for one scope cannot lease another's job; a log read path cannot reach across jobs. | `internal/runner`, `internal/actions` (phase 5 W5.15) |
| T-H9 | Exfiltration to the network: a job reaching internal services, the database, cloud metadata, or the instance's own API as a privileged caller. | Default-deny egress for the job network with a configurable allowlist; no route to the database network; cloud metadata addresses blocked; the workflow token is the only credential available and it is repository-scoped and expiring. | `internal/platform/container`, `internal/runner` (phase 5) |
| T-H10 | Resource abuse: infinite jobs, fork bombs, unbounded logs or artifacts, or jobs that consume the scheduling budget. | Explicit CPU, memory, disk, process-count, and wall-clock limits per job, plus bounded log and artifact sizes with explicit truncation markers; concurrency groups limit one running and one pending run per group; per-user and per-repository quotas for concurrent jobs and job minutes are enforced before scheduling with a clear rejection reason. | `internal/actions`, `internal/runner` (phase 5 W5.12, W5.15) |
| T-H11 | Orphaned containers or data after a crash, retaining secrets or consuming resources. | Job containers are created fresh per job and removed on completion, failure, timeout, and cancel; a reconciler removes containers and networks left by a worker crash; cleanup is restarted at worker startup. | `internal/runner`, `internal/platform/container` |
| T-H12 | A compromised or malicious self-hosted runner acting on the control plane. | Registration tokens are single-use, expiring, and scoped to repository, organization, or instance; label matching is enforced by the scheduler; a runner can report results and fetch work only for leases it holds; a runner older than the supported minimum is refused at registration with a distinguishable error. | `internal/runner` (phase 5 W5.15) |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: workflow on a push to a branch with a read-only default token and a repository secret | Job runs to completion; the secret is available to the step and absent from logs, artifacts, and the stored run definition. |
| Allowed: job uploads an artifact and a later job in the same repository downloads it | Download succeeds; the artifact is not reachable from another repository. |
| Allowed: cache save on `main` and restore on `main` with an exact key | Restore hits; a restore from a different repository and from a fork misses. |
| Denied: step printing a secret, its base64 form, and a JSON-escaped form | Stored and streamed logs contain the mask in every case. |
| Denied: fork pull request requesting a repository secret through `secrets: inherit` | No secret injected; the token is read-only; the run records the reduced privilege. |
| Denied: workflow with `permissions: contents: write` when repository policy forbids widening | Token is not widened; the job receives the default read-only set and a clear failure for the disallowed request. |
| Denied: unpinned `uses: owner/action@main`, recursive composite action, and a workflow YAML with duplicate keys | Rejected at parse or resolution time before any step runs. |
| Denied: job attempting to reach PostgreSQL, the runtime socket, `169.254.169.254`, and another repository's cache API | All connections or requests fail; no credential is available to retry with. |
| Denied: job with a fork bomb, a 50 GB log stream, and a 1-hour wall clock | Job is terminated at its limit; log is truncated with an explicit marker; container removed. |
| Crash: kill the worker mid-job | Lease expires, job is retried exactly once, and no container, network, or volume from the crashed attempt remains. |
| Denied: runner presenting another scope's registration token or leasing another runner's job | Registration or lease refused with a distinguishable error. |

## 11. Surface (i): cloud-development workspaces

**Assets at risk:** workspace volumes and snapshots containing tenant source and data; injected credentials; the container runtime and host network; the IDE and terminal session tokens; preview URLs; the workspace image supply chain.

**Attacker capabilities assumed:** a user who controls a devcontainer configuration, lifecycle commands, dotfiles, repository code, and the processes inside their workspace; a user attempting to reach another user's workspace, volume, snapshot, or forwarded port; a hostile or compromised image or feature referenced by a devcontainer configuration; a browser client presenting a stolen or replayed IDE session.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-I1 | Tenant isolation failure: one workspace reading another's volume, snapshot, image layer, or environment, or reaching another's container on the host network. | One container and one volume per workspace, keyed by the workspace UUID; identifiers are resolved from database rows bound to the requesting workspace; a per-workspace network with no route to other workspace networks, the runtime socket, or the host; workspace containers never receive database credentials or the instance secret key. | `internal/workspace`, `internal/platform/container` (phase 9) |
| T-I2 | Volume or snapshot cross-read during rebuild, restore, or snapshot operations. | Rebuild, restore, and snapshot resolve the volume from the workspace row and the requesting principal; a reset flag creates a new volume rather than reusing another's; image and snapshot identifiers are never client-supplied. | `internal/workspace` (phase 9 W9.4, W9.12) |
| T-I3 | Credential exposure: a Git token, platform credential, or secret left in the image, the volume, shell history, environment of a later session, or browser storage. | Credentials are injected at start or for the specific operation, kept out of the image and the persistent volume, and removed after use; Git access from inside a workspace uses a short-lived scoped token, never a long-lived personal access token; no credential is placed in browser local storage or any URL; a deleted user or revoked session terminates existing IDE and terminal sessions within a bounded time. | `internal/workspace` (phase 9 W9.5, W9.8, W9.13) |
| T-I4 | Port-forwarding and preview-URL authorization failure: an unauthenticated or unauthorized caller reaching a forwarded service, refusing in a way that discloses whether a port exists, or reaching a client-supplied host address or port. | Access to a preview URL follows an explicit policy (private to the owner by default, optionally shared, optionally a signed expiring link, never public unless an operator enables it); refusal does not disclose port existence; routing resolves the workspace from the signed route or verified host mapping, never from a client-supplied container address or port. | `internal/workspace` (phase 9 W9.7) |
| T-I5 | Session hijack on the IDE or terminal channel: a stolen session token replayed, a handshake authorized once and then trusted, or a WebSocket route bypassing identity. | The IDE and terminal routes verify identity and workspace scope on every attach, not only the initial handshake; the IDE session token is short-lived, workspace-scoped, and single-use; attach and detach are recorded in the audit log with actor, workspace, and time. | `internal/workspace` (phase 9 W9.5, W9.6) |
| T-I6 | Image supply chain: a mutable tag swapped after review, an unverified image or `feature`, a build context or mount source outside GitNest's control, or a poisoned prebuild. | Builds are keyed by a content hash of the resolved configuration and build inputs; images and features are pinned by version or digest and pulled from configured registries under a digest policy; `mounts` and build contexts are constrained to sources GitNest controls, and a mount outside the workspace or an allowlisted cache root is rejected at parse time; a prebuild is only reused when its key matches. | `internal/workspace` (phase 9 W9.2, W9.9) |
| T-I7 | Control-plane injection from workspace output: container output, forwarded-port metadata, or a lifecycle command interpreted as a command, path, or configuration value by GitNest. | The control plane never executes code from inside a workspace; container output is parsed defensively, bounded in size, and treated as data; devcontainer JSON, dotfiles commands, and lifecycle commands are untrusted input validated against the same rules as workflow input. | `internal/workspace` (phase 9 W9.2, W9.8) |
| T-I8 | Resource abuse: a workspace consuming host CPU, memory, disk, or lifetime beyond its quota, or exhausting the instance so others cannot start. | CPU, memory, pids, disk, wall-clock lifetime, and idle timeouts are mandatory and enforced by the control plane, not by the workspace; a workspace over its limits is stopped by the control plane; per-user, per-repository, and instance quotas gate starts and are checked before scheduling; idle and expired workspaces are stopped before new starts are refused. | `internal/workspace` (phase 9 W9.10) |
| T-I9 | Orphaned containers, volumes, or networks after a crash retaining data or credentials. | Workspace containers, networks, and volumes carry a workspace label so a reconciler finds and removes orphans after a crash; cleanup on stop, delete, failed create, timeout, and process restart is deterministic. | `internal/workspace`, `internal/platform/container` (phase 9 W9.3) |
| T-I10 | Cross-tenant content in logs, metrics, or the audit trail. | Logs, metrics, and audit records never contain workspace file contents, secret or token values, or terminal input; the audit trail records that a session occurred and by whom, not what was typed. | `internal/platform/logging`, `internal/activity` |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: create a workspace from a repository ref, start it, reach the IDE and terminal, forward a port, stop, resume with the volume intact | All steps succeed; the volume keeps its contents across stop and start. |
| Allowed: create a scratch workspace with no repository | Starts with an empty volume and no repository credential. |
| Allowed: owner opens a shared preview link as an authorized collaborator | Request succeeds under the sharing policy. |
| Denied: workspace A requests workspace B's volume ID, snapshot ID, or container address | Not-found; no volume, snapshot, or container is touched. |
| Denied: request to a forwarded port from an unauthenticated client, from a user without share access, and for a port that does not exist | All three return the same refusal shape with no port-existence disclosure. |
| Denied: reuse of an IDE session token after logout, after user deletion, and for a different workspace | All rejected; existing terminal sessions terminate within the documented bound. |
| Denied: devcontainer `mounts` pointing at the host root, the runtime socket, or another workspace's volume | Rejected at parse time; no container is created. |
| Denied: image referenced by mutable tag with a digest policy configured | Build refused with a distinguishable error. |
| Denied: workspace exceeding CPU, memory, disk, and lifetime limits | Control plane stops the container at the limit and records the reason. |
| Crash: kill the workspace agent while containers run | Reconciler removes the labelled containers, networks, and orphan volumes within the documented window. |

## 12. Surface (j): Pages builds and serving

**Assets at risk:** the application origin and the sessions of users who view a Pages site; the serving root and the host filesystem; the custom-domain namespace; the build environment (which runs repository-controlled commands).

**Attacker capabilities assumed:** a repository owner who controls the site content, build command, and artifact layout, including symlinks and hostile file names; a user claiming a custom domain they do not control, or re-claiming a lapsed domain; any unauthenticated visitor of a public site.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-J1 | Stored XSS on the served site, or on the GitNest origin if content is served from the same host. | A Pages site is served from its own host (or the configured Pages host pattern) and never from the session origin; response headers set a restrictive content-security policy, no credential or session cookie is valid on the Pages host, and the build artifact is treated as untrusted content. | `internal/pages` (phase 7 W7.11) |
| T-J2 | Path traversal and symlink escape from the artifact to the host filesystem, or directory listing exposing unintended files. | The served path is resolved inside the artifact root for the site record, symlinks are not followed outside it, `..` segments are rejected after normalization, and directory listing is disabled. | `internal/pages` (phase 7 W7.11) |
| T-J3 | Custom-domain takeover: an unverified domain served, one domain claimed twice, a lapsed domain still serving, or a `CNAME` pointing at GitNest from an abandoned site. | Serving requires a stored site record and proof of control (a domain-verification token checked before serving); a domain cannot be claimed by two owners; verification is rechecked on renewal so a lapsed domain stops serving; an unverified host returns a distinguishable "unconfigured" response with no site content. | `internal/pages` (phase 7 W7.12) |
| T-J4 | Host-header abuse: a request with a crafted `Host` served the wrong site, or used to satisfy domain verification. | The target site is resolved from the verified host mapping and the stored record, never from a client-supplied path under the serving root; verification checks are not satisfiable from a request header. | `internal/pages` (phase 7 W7.12) |
| T-J5 | Build-time compromise: the build command reading the application database, repository storage outside the checkout, the instance secret key, or another tenant's artifacts. | The Pages build runs through the phase 5 runner boundary with resource limits and no access to the application's database credentials or repository storage outside the checkout, or it is disabled entirely when no runner is configured. | `internal/pages` (phase 7 W7.11) |
| T-J6 | Artifact abuse: a huge or deeply nested artifact filling storage, or a build that never terminates. | Build wall-clock, CPU, memory, and artifact-size limits; artifact retention and count caps per repository; a failed or timed-out build publishes nothing and leaves no partial artifact. | `internal/pages` (phase 7 W7.11) |
| T-J7 | Traffic or telemetry data exposing visitor information. | Traffic counters store no credentials, request bodies, or client content beyond what retention explicitly covers; retention limits are enforced by a job; counters are aggregated, not per-visitor profiles. | `internal/pages` (phase 7 W7.15) |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: publish an artifact for a verified custom domain and serve it over HTTPS | Site serves from the verified host with the expected security headers and no GitNest session cookie accepted. |
| Allowed: site rebuild after a new artifact is published | New content serves; the previous artifact is replaced according to retention. |
| Denied: request for `/../../etc/passwd`, an encoded traversal, a symlink to the config directory, and a directory path | All return not-found; no file outside the artifact root is read; directory listing is disabled. |
| Denied: request with a `Host` for an unverified domain, and a request attempting to complete domain verification through a header | No site content served; verification not satisfied. |
| Denied: second owner verifying a domain already bound to another site | Rejected; the existing binding is unchanged. |
| Denied: domain whose verification has lapsed on renewal | Serving stops; the site reports an unconfigured state. |
| Denied: build command attempting network access to the database, the GitNest API, and the instance secret key | No credential is present; the attempt fails; the run is recorded as a failed build. |
| Denied: build producing a 100 GB artifact and a build that exceeds its time limit | Build fails within the bound; no partial artifact is published. |
| Denied: site content containing `<script>` served to a visitor | Script executes only within the Pages origin, cannot read a GitNest session, and cannot call the authenticated API as the viewer. |

## 13. Surface (k): authentication surfaces

**Assets at risk:** passwords and password hashes; browser sessions and CSRF tokens; personal access tokens and SSH keys; second factors (TOTP seeds, WebAuthn credentials, recovery codes); the account recovery path; the invitation flow; SAML and SCIM credentials at the organization level.

**Attacker capabilities assumed:** an unauthenticated remote client issuing credential guesses and session-fixation attempts; a network attacker without TLS termination control; a malicious page in the victim's browser issuing cross-site requests; a caller who obtained a token or a recovery code and replays it; a user who reached the first factor and attempts to bypass the second; an attacker with control of a mail delivery path.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-K1 | Brute force and credential stuffing against sign-in, password reset, invitation acceptance, 2FA verification, and token endpoints. | Rate limits keyed by credential and by source address, with stricter budgets for failures, applied to every authentication entry point including API token authentication; generic failure responses that do not distinguish unknown account from wrong password; recorded authentication audit events. | `internal/identity` limiters, phase 4 W4.4 |
| T-K2 | Account enumeration through responses, timing, or the invitation and password-reset flows. | Uniform response shape and comparable timing for existent and nonexistent accounts; the same not-found response for private resource disclosure; no confirmation that an email address belongs to an account. | `internal/identity` |
| T-K3 | Session fixation and session hijack: a pre-set session identifier surviving login, a session readable by script, or a session that outlives revocation. | A new opaque session token issued on every authentication and on privilege change; sessions stored hashed server-side; cookies `Secure`, `HttpOnly`, `SameSite=Lax`; no session token in browser storage or a URL; revocation takes effect on the next request. | `internal/identity`, phase 1 W1.4 |
| T-K4 | CSRF on state-changing browser requests, including login CSRF on the sign-in form. | A CSRF token required on every state-changing request, bound to the session, and verified before the handler runs; `SameSite=Lax` as defence in depth, not as the control; the sign-in and consent forms carry a CSRF token too. | `internal/identity`, `internal/transport/http/web` |
| T-K5 | Token replay: a stolen session cookie, personal access token, OAuth token, or invitation token reused. | Tokens are opaque, high-entropy, stored as digests, compared in constant time, and carry expiry; invitation and password-reset tokens are single use; personal access tokens record last use and are individually revocable; session cookies are not accepted on cross-origin requests and TLS is mandatory for non-loopback installs. | `internal/identity`, `internal/authapp` |
| T-K6 | 2FA bypass: reaching a state-changing or credential-management action with only the first factor, downgrading the factor, or replaying a TOTP code. | Sensitive operations require a completed second factor for the session, verified at the service; a TOTP code is accepted once per time step per account; factor removal and recovery-code regeneration require re-authentication; WebAuthn relying-party ID and origin derive from the configured public URL, never request headers, and the signature counter is checked. | `internal/identity` (phase 8 W8.8, W8.9) |
| T-K7 | Recovery-code abuse: codes stored in plaintext, reusable, unbounded in number, or usable without the first factor. | Recovery codes are generated once, shown once, stored hashed, single use, invalidated on use and on factor change, and usable only after the first factor succeeds; regeneration invalidates all previous codes. | `internal/identity` (phase 8 W8.10) |
| T-K8 | Account recovery as a takeover path: an operator or support flow that resets a password or disables 2FA without authorization or an audit record. | Password reset is by single-use expiring token delivered to the verified address, never by revealing a password; an administrator-initiated reset requires an authenticated administrator session, and both the request and the completion are audited; disabling 2FA for an account is an audited administrative action with an explicit confirmation and, where the account is protected by organization policy, is refused. | `internal/identity` (phase 8 W8.10, `cmd/gitnest` admin commands) |
| T-K9 | Invitation abuse: an invitation token guessed, forwarded to the wrong recipient, or accepted for a higher role than intended. | Invitation tokens are single use, expiring, stored as digests, bound to the invited address and the intended role, and revocable; acceptance creates the account with the bound role only. | `internal/identity`, `internal/organization` |
| T-K10 | SSO and provisioning abuse: a forged or replayed SAML assertion, a widened SCIM mapping, or a SCIM token used beyond its scope. | SAML assertions are validated for signature, audience, recipient, issue and expiry instants, and replay; IdP certificates are pinned and rotation is an explicit operator action; SCIM tokens are per-organization, hashed, narrow in scope, and revocable; a SCIM write can never grant a role beyond the mapping's ceiling. | `internal/organization` (phase 8 W8.12, W8.13) |
| T-K11 | Operator bootstrap abuse: a remotely reachable setup flow, a second bootstrap administrator, or a weak bootstrap password. | The first administrator is created only by the local CLI, in one transaction guarded by an advisory lock, with password rules validated before hashing and no setup route to disable; a repeated bootstrap is refused. | `cmd/gitnest`, `internal/identity` |
| T-K12 | Second-factor material leaking: TOTP seeds, WebAuthn credential IDs, and recovery codes appearing in logs, API responses, exports, or audit detail. | Second-factor material is encrypted at rest with the instance key and excluded from logs, API responses, exports, and audit detail fields. | `internal/identity` (phase 8) |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Allowed: sign in with a correct password and a valid TOTP code, then reach a sensitive settings action | Session is created with a fresh token; sensitive action is permitted only after the second factor. |
| Allowed: use one recovery code once and sign in | Code accepted once; the second use of the same code fails; remaining codes are unaffected. |
| Allowed: accept a valid invitation bound to the invited address | Account created with the bound role only. |
| Denied: 20 failed sign-ins from one address and from one account across 20 addresses | Both budgets throttle at the documented threshold; responses remain generic. |
| Denied: sign-in request with a pre-set session cookie and a cross-site `POST` without a CSRF token | Cookie is replaced on authentication; the CSRF-less request is rejected before the handler. |
| Denied: replay of a consumed TOTP step, a consumed recovery code, and a consumed invitation token | All rejected with the same failure shape as an invalid credential. |
| Denied: sensitive settings action with only the first factor | Denied at the service; no state change. |
| Denied: forged SAML assertion, assertion replayed after expiry, and a SCIM token from another organization | All rejected; no membership change. |
| Denied: `gitnest admin create` run a second time, and run concurrently four times | Refused after the first success; concurrent runs leave exactly one administrator. |
| Denied: sign-in with a response body and timing for an unknown account compared to a wrong password | Response shape and timing classes are indistinguishable in the test's assertion. |

## 14. Surface (l): denial of service and resource exhaustion

**Assets at risk:** request-handling capacity; database connections and locks; worker and queue capacity; disk on the node; the Git process table; the audit trail's availability.

**Attacker capabilities assumed:** an unauthenticated client issuing many requests or a few very expensive ones; an authenticated client with a valid token doing the same within its rate budget; an attacker spreading requests across addresses and accounts; a repository crafted to make a single request expensive.

| # | Threat | Required mitigation | Where enforced |
| --- | --- | --- | --- |
| T-L1 | Expensive single requests: an archive of a huge repository, a diff of two unrelated trees, a code search over a large corpus, or a workspace start. | Every expensive operation has a documented byte, entry, time, and concurrency cap enforced at the service or platform adapter, with a clear error and an explicit truncation marker rather than unbounded work. | Domain services, `internal/platform/git`, `internal/search` |
| T-L2 | Request volume and expensive-endpoint abuse. | Rate limits per credential and per source address, separate budgets for authenticated, unauthenticated, and app-installation callers, stricter budgets for expensive endpoints, connection and read-header timeouts, and request-size limits. | Rate-limit service, `internal/transport/http/*` |
| T-L3 | Database exhaustion: connection-pool starvation, long-running queries, lock waits, or an unbounded list query. | A bounded connection pool with acquisition timeouts, statement timeouts on request-path queries, pagination with a maximum page size and an enforced default, and lock acquisition with a timeout so one tenant cannot hold a repository lock indefinitely. | `internal/platform/postgres` |
| T-L4 | Queue and worker exhaustion: a job storm, a poison job retried forever, or a webhook target that never responds. | Bounded retries with capped backoff and a dead-letter state, per-class concurrency limits, a visibility timeout so a crashed worker's job returns to the queue, and priority classes that keep interactive notification work from starving behind bulk indexing. | `internal/jobs` |
| T-L5 | Disk exhaustion: logs, artifacts, caches, LFS objects, package blobs, page builds, uploads, or temporary Git and archive data. | Quotas and retention on every stored class, a free-space check before a large write, temporary roots with a startup sweep of stale data, and a documented refuse-write threshold so the instance fails predictably instead of corrupting state. | `internal/platform/storage`, quota services |
| T-L6 | Process-table exhaustion from Git, archive, and build processes. | Per-instance and per-repository limits on concurrent Git operations and builds, process-group kill on timeout or cancellation, and a reaper that terminates processes whose request has ended. | `internal/platform/git`, `internal/runner` |
| T-L7 | Account- or tenant-level starvation: one tenant's workload consuming the instance's shared budget. | Quotas and priorities are attributed to the actor (user, organization, repository, installation), not only to the whole instance, so one tenant cannot exhaust shared capacity for others. | Quota services across domains |
| T-L8 | Amplification through caching or retries: a cache stampede after an invalidation, or retries multiplying load on a failing dependency. | Single-flight or jittered cache fills with bounded entry counts, retry with backoff and jitter and a maximum attempt count, and circuit-breaking behaviour that degrades to the fallback (for example SQL search) rather than failing closed. | `internal/platform/cache`, `internal/jobs`, `internal/search` |

**Test obligations**

| Case | Expectation |
| --- | --- |
| Denied: archive, diff, and search requests exceeding their caps | Each returns a bounded response with an explicit marker or a clear error within the documented time. |
| Denied: request body and header larger than the configured limit | Rejected before the handler runs; the connection is closed cleanly. |
| Denied: 1 000 concurrent clones of one repository | Concurrency limit is enforced; excess requests wait or receive a distinct busy error; no request fails with an internal error. |
| Denied: list endpoint requesting `per_page=100000` and walking deep pagination | Page size is capped at the documented maximum; deep pagination is bounded and returns a stable result set. |
| Denied: poison job that fails on every attempt | Job reaches the dead-letter state after the documented attempt count and stops consuming worker capacity. |
| Denied: webhook target that never responds under a subscription storm | Delivery attempts are bounded and queued; unrelated queue classes continue to drain. |
| Denied: writes when free disk is below the reserve threshold | Write is refused with a clear operator-visible error; no partial artifact, blob, or repository state remains. |
| Load: a mixed workload (clone-heavy, push-heavy, browse-heavy, many concurrent jobs) | Queue depth, oldest-job age, and error rate stay within the documented budgets for the instance size; no unbounded growth in memory, process count, or open file handles. |
| Revocation under load: revoke a session and a token during the workload | The next request with the revoked credential fails in every transport. |

## 15. Threat to mitigation to enforcement to test

Rows are grouped by surface; the test column names the package or case that must prove the mitigation.

| Threat | Mitigation | Where enforced | Test |
| --- | --- | --- | --- |
| T-A1 command injection | Argument arrays, service allowlist, no shell | `internal/platform/git` | `internal/platform/git`: metacharacter and unknown-service cases start no process |
| T-A2 path traversal | ID-derived paths, root containment, name grammar | `internal/platform/storage`, `internal/repository` | `internal/platform/storage`: traversal and symlink cases |
| T-A3 cross-transport bypass | One authorization path for HTTP and SSH | `internal/gittransport` | `internal/gittransport`, `internal/transport/ssh`: denied read and write cases |
| T-A4 existence disclosure | Uniform not-found shape and timing | `internal/gittransport` | `internal/transport/http/git`: private and nonexistent repository comparison |
| T-A5 hook or service abuse | No repository hooks, service allowlist, controlled environment | `internal/platform/git` | `internal/platform/git`: unexpected service refused, no hook executed |
| T-A6 environment exposure | Minimal explicit environment | `internal/platform/git` | `internal/platform/git`: child environment assertion |
| T-A7 ref-update race | Compare-and-swap, advisory locks | `internal/platform/git`, `internal/pullrequest` | `internal/pullrequest` and `internal/platform/git`: concurrent push cases |
| T-A8 Git resource exhaustion | Timeouts, byte caps, process-group kill, cleanup | `internal/platform/git` | `internal/platform/git`: cancelled and timed-out process reaped, temp root empty |
| T-A9 SSH surface abuse | Key-only, no PTY or forwarding, repository-bound keys | `internal/transport/ssh` | `internal/transport/ssh`: PTY, forwarding, deploy-key scope cases |
| T-A10 Git credential logging | Redaction of URL and header credentials | `internal/platform/logging` | `internal/platform/logging`: redaction case with credentialed clone URL |
| T-B1 stored XSS | Raw HTML disabled, sanitizer, protocol allowlist | Markdown renderer | Renderer test: script, handler, and protocol cases |
| T-B2 attribute or CSS context XSS | Contextual encoding, grammar-validated values | Renderer, React components | Renderer test: attribute and color injection cases |
| T-B3 tree and raw-view traversal | Resolve inside the requested tree; do not follow symlinks out | `internal/repository` | `internal/repository`: symlink and traversal cases |
| T-B4 diff rendering abuse | Control-character stripping, line-length cap, binary summary | Diff renderer | Diff renderer test: control, RTLO, and long-line cases |
| T-B5 cache cross-viewer leak | Permission-aware cache key | `internal/platform/cache` | Cache test: two principals with different permissions |
| T-B6 external resource embedding | Allowlist or proxy, CSP, referrer policy | `internal/transport/http/web` | Web test: header assertion and disallowed embed case |
| T-B7 template injection | Escaped server-side interpolation, no evaluation | Renderer | Renderer test: brace, backtick, and `$()` payloads remain inert |
| T-C1 archive bomb | Streaming with byte, entry, and time caps | `internal/gittransport` | Archive cap test: oversized tree within the bound |
| T-C2 header injection | RFC 6266 encoding of generated names | `internal/transport/http/git` | Header test: CR, LF, and quote in ref name |
| T-C3 LFS enumeration | Authorization per repository in the batch | `internal/gittransport` | LFS test: cross-repository batch and object cases |
| T-C4 LFS integrity | Digest and size verification before finalize | `internal/platform/storage` | LFS test: mismatched digest and size |
| T-C5 staging leak | Per-operation temp root with startup sweep | `internal/platform/storage` | Storage test: temp root absent after success, failure, and restart |
| T-C6 archive concurrency | Shared rate and concurrency budget | Rate-limit service | Rate-limit test: archive concurrency budget |
| T-D1 upload path traversal | Generated names from IDs and digests | `internal/platform/storage` | Upload test: traversal-name and NUL-byte cases |
| T-D2 decode bomb | Bounded decoder with output cap | Upload processing call sites | Upload test: pixel-count and expansion-ratio bombs |
| T-D3 content-type confusion | Stored validated type or forced download, `nosniff` | Upload serving path | Upload test: declared-versus-stored type assertion |
| T-D4 quota and disk exhaustion | Quota check under lock, free-space check | Quota service, `internal/platform/storage` | Upload test: quota-exceeded and low-free-space cases |
| T-D5 same-origin active content | Separate origin, download disposition, or sanitize | Upload serving path | Upload test: SVG and HTML script case on the session origin |
| T-D6 orphaned or guessable upload | Owner and target reference, permission recheck on read | Domain services | Upload test: deleted-target and cross-tenant read cases |
| T-E1 credential guessing and enumeration | Hashed storage, constant-time compare, generic errors | `internal/identity`, `internal/authapp` | Identity test: unknown, expired, and revoked credential classes |
| T-E2 scope escalation | One decision function composing all grant kinds | Authorization decision service | Decision-service test: every principal kind at each permission level |
| T-E3 object reference substitution | Resolve through the owning service under the principal | Domain services | API test: cross-tenant identifier cases |
| T-E4 over-posting | Explicit request structs and settable-field allowlist | `internal/transport/http/api` | API test: protected field in a request body |
| T-E5 response leakage | Explicit response structs, shared error mapper, filtered counts | `internal/transport/http/api` | API test: response field and pagination-total cases |
| T-E6 GraphQL bypass | Service-backed resolvers, depth and complexity limits, introspection off | `internal/transport/http/api` | GraphQL test: depth, complexity, private resolver, introspection cases |
| T-E7 OAuth and app abuse | Exact redirect match, single-use `state`, PKCE, short-lived scoped tokens | `internal/authapp` | Authapp test: redirect, state, PKCE, token-expiry cases |
| T-E8 credential replay and revocation | One revocation path, no transport-local cache, expiry | `internal/identity` | Cross-transport revocation test in API, Git HTTP, and SSH |
| T-E9 rate-limit evasion | Credential-keyed shared buckets, expensive-endpoint budgets | Rate-limit service | Rate-limit test: many addresses, one credential |
| T-E10 audit gaps | Audit event in the same transaction as the change | `internal/activity` | Activity test: credential change without an audit row fails the transaction |
| T-F1 SSRF to internal targets | Resolve-validate-connect, redirect disabled, allowlist | `internal/webhooks` | Webhook test: loopback, metadata, RFC1918, and allowlist cases |
| T-F2 DNS rebinding | Pinned resolution | `internal/webhooks` | Webhook test: two-answer resolver |
| T-F3 signature forgery | HMAC over transmitted bytes, constant-time compare | `internal/webhooks` | Webhook test: tampered body and re-serialized body |
| T-F4 replay | Unique delivery GUID and timestamp, documented receiver defence | `internal/webhooks` | Webhook test: distinct GUIDs per attempt and record fields |
| T-F5 slow or oversized target | Timeouts, attempt cap, bounded stored body prefix | `internal/webhooks` | Webhook test: non-responding and oversized-response targets |
| T-F6 secret disclosure in delivery records | Store digest and headers, never the secret value | `internal/webhooks` | Webhook test: delivery record field assertion |
| T-F7 retry storm | Bounded backoff, attempt cap, per-subscription scope, disable on terminal failure | `internal/jobs`, `internal/webhooks` | Jobs test: retry schedule and terminal state |
| T-F8 self-targeting | Detect the instance's own addresses and public URL | `internal/webhooks` | Webhook test: delivery to the configured public URL |
| T-G1 registry path and key traversal | Grammar validation, ID and digest derived keys | `internal/packages` | Packages test: hostile name and version cases |
| T-G2 name squatting | Owner namespace, reserved names, audited transfer | `internal/packages`, `internal/organization` | Packages test: foreign-namespace publish and reserved-name cases |
| T-G3 blob integrity | Digest verify on upload and scrub, content addressing | `internal/packages` | Packages test: mismatch and scrub-quarantine cases |
| T-G4 malicious archive or metadata | Bounded parse, no extraction outside temp, no execution | `internal/packages` | Packages test: traversal tarball, zip bomb, and nested metadata |
| T-G5 unauthorized registry access | Shared decision function for every registry verb | `internal/packages` | Packages test: read-only push, foreign delete, private pull |
| T-G6 quota bypass | Lock-protected accounting, retention dry run | `internal/packages` | Packages test: concurrent uploads at the byte limit |
| T-G7 registry credential leakage | Redaction of URLs, headers, and challenge echoes | `internal/packages`, `internal/platform/logging` | Logging test: registry request with a bearer token |
| T-G8 download URL scope | Per-decision, short-lived, one-blob URL | `internal/packages` | Packages test: URL reuse across owners |
| T-H1 job credential exfiltration | Separate trust domain, no DB credential, no host mounts | `internal/runner`, `internal/platform/container` | Runner test: environment and mount assertion in a job container |
| T-H2 secret leakage | Masker over every storage and streaming path, including failure paths | `internal/actions` | Actions test: base64, multi-line, JSON-escaped secret forms |
| T-H3 cache poisoning | Repository and branch scoping, fork write separation, caps | `internal/actions` | Actions test: cross-repository and fork cache cases |
| T-H4 workflow-token escalation | Least privilege by default, explicit widening, repository scope, run-end expiry | `internal/actions` | Actions test: permission matrix and token lifetime |
| T-H5 fork pull-request abuse | No secrets, read-only token, shared protection check | `internal/actions` | Actions test: fork run secret and token assertions |
| T-H6 malicious action reference | SHA pinning, recorded resolution, registry digest policy | `internal/actions` | Actions test: unpinned ref, recursion, digest policy |
| T-H7 untrusted-input parsing | Strict YAML, bounded expression evaluator, grammar-validated names | `internal/actions` | Actions test: duplicate keys, huge inputs, hostile names |
| T-H8 cross-tenant job leakage | Per-run authorization for logs, artifacts, caches, leases | `internal/runner`, `internal/actions` | Runner test: cross-run log, artifact, and lease cases |
| T-H9 job egress | Default-deny egress with allowlist, no database route | `internal/platform/container` | Runner test: blocked internal and metadata addresses |
| T-H10 job resource abuse | Mandatory limits, concurrency groups, job quotas | `internal/actions`, `internal/runner` | Actions test: fork bomb, log cap, quota rejection |
| T-H11 orphaned job resources | Fresh container per job, crash reconciler | `internal/runner` | Runner test: kill the worker and assert removal |
| T-H12 malicious runner | Single-use scoped registration tokens, lease binding, version floor | `internal/runner` | Runner test: cross-scope registration and lease |
| T-I1 workspace tenant isolation | One container and volume per workspace, per-workspace network | `internal/workspace` | Workspace test: cross-workspace access matrix |
| T-I2 volume and snapshot cross-read | Resolve from the workspace row and principal | `internal/workspace` | Workspace test: foreign volume and snapshot identifiers |
| T-I3 workspace credential exposure | Inject per operation, never persist, short-lived scoped Git token | `internal/workspace` | Workspace test: post-stop scan of volume, environment, and browser storage |
| T-I4 port-forward authorization | Explicit access policy, no existence disclosure, verified route mapping | `internal/workspace` | Workspace test: unauthenticated, unauthorized, and absent port |
| T-I5 IDE and terminal session hijack | Verify on every attach, short-lived single-use token, audit events | `internal/workspace` | Workspace test: replay, logout, deletion, cross-workspace |
| T-I6 image supply chain | Content-hash keys, version or digest pinning, constrained mounts | `internal/workspace` | Workspace test: mutable tag, disallowed mount, prebuild key mismatch |
| T-I7 control-plane injection | Workspace output treated as bounded data | `internal/workspace` | Workspace test: hostile port metadata and lifecycle output |
| T-I8 workspace resource abuse | Mandatory limits enforced by the control plane, quotas before start | `internal/workspace` | Workspace test: limit enforcement and quota rejection |
| T-I9 orphaned workspace resources | Labelled resources with a crash reconciler | `internal/workspace` | Workspace test: agent crash cleanup |
| T-I10 workspace content in logs | No file, secret, or terminal-input content in logs or metrics | `internal/platform/logging` | Logging test: workspace session redaction |
| T-J1 Pages stored XSS | Separate host, no session cookie, restrictive CSP, untrusted content | `internal/pages` | Pages test: script content cannot read a GitNest session |
| T-J2 Pages traversal | Resolve inside the artifact root, no symlink escape, no listing | `internal/pages` | Pages test: traversal, symlink, and directory cases |
| T-J3 custom-domain takeover | Stored site record, proof of control, unique claim, renewal recheck | `internal/pages` | Pages test: unverified, duplicate, and lapsed domain cases |
| T-J4 Host-header abuse | Resolve from the verified host mapping | `internal/pages` | Pages test: crafted `Host` and header-based verification |
| T-J5 build-time compromise | Build through the runner boundary with no platform credentials | `internal/pages` | Pages test: build reaching for the database and secret key fails |
| T-J6 artifact abuse | Build and artifact limits, retention caps, no partial publish | `internal/pages` | Pages test: oversized artifact and timed-out build |
| T-J7 telemetry exposure | Aggregated counters, no credentials or bodies, retention job | `internal/pages` | Pages test: stored counter fields and retention deletion |
| T-K1 brute force | Failure-weighted rate limits on every authentication entry point | `internal/identity` | Identity test: per-address and per-account budgets |
| T-K2 account enumeration | Uniform response shape and timing classes | `internal/identity` | Identity test: unknown account versus wrong password |
| T-K3 session fixation | Fresh token on authentication and privilege change, hashed server-side | `internal/identity` | Identity test: pre-set cookie replaced; cookie flag assertion |
| T-K4 CSRF | Session-bound CSRF token verified before the handler | `internal/identity`, `internal/transport/http/web` | API test: state-changing request without a token |
| T-K5 token replay | Opaque high-entropy tokens, digest storage, expiry, single use | `internal/identity` | Identity test: consumed invitation and reset token |
| T-K6 2FA bypass | Second factor required at the service for sensitive actions, single TOTP step, RP ID from public URL | `internal/identity` | Identity test: first-factor-only action, replayed code, wrong origin |
| T-K7 recovery-code abuse | Hashed, single-use, regenerated as a set | `internal/identity` | Identity test: reuse and regeneration cases |
| T-K8 recovery as takeover | Single-use expiring reset token; audited administrative reset | `internal/identity`, `cmd/gitnest` | Identity test: reset token lifetime and audit row |
| T-K9 invitation abuse | Single-use, expiring, address- and role-bound invitation | `internal/identity` | Identity test: wrong address and reused invitation |
| T-K10 SSO and SCIM abuse | Assertion validation with pinning, narrow revocable SCIM tokens | `internal/organization` | Organization test: forged, replayed, and over-scoped cases |
| T-K11 bootstrap abuse | Local CLI only, advisory-lock transaction, password rules first | `cmd/gitnest`, `internal/identity` | `cmd/gitnest/admin_test.go`: second and concurrent bootstrap |
| T-K12 second-factor material leakage | Encrypted at rest, excluded from responses, exports, and logs | `internal/identity` | Identity test: response, export, and log field assertions |
| T-L1 expensive single requests | Documented caps on every expensive operation | Domain services, `internal/platform/git` | Cap tests per surface (archive, diff, search, workspace start) |
| T-L2 request volume | Rate limits, timeouts, request-size limits | Rate-limit service, transports | Rate-limit and transport test: limit, timeout, and oversized body |
| T-L3 database exhaustion | Bounded pool, statement timeout, page-size cap, lock timeout | `internal/platform/postgres` | Postgres test: acquisition timeout, lock timeout, page cap |
| T-L4 queue exhaustion | Bounded retries, dead-letter, visibility timeout, priorities | `internal/jobs` | Jobs test: poison job, crashed worker, priority ordering |
| T-L5 disk exhaustion | Quotas, retention, free-space check, stale-temp sweep | `internal/platform/storage` | Storage test: reserve threshold, sweep, retention deletion |
| T-L6 process exhaustion | Concurrency limits and reapers for Git and build processes | `internal/platform/git`, `internal/runner` | Concurrency test: excess processes queued or refused; reaper case |
| T-L7 tenant starvation | Actor-attributed quotas and priorities | Quota services | Quota test: one tenant cannot consume the instance budget |
| T-L8 amplification | Single-flight fills, jittered retry, fallback degradation | `internal/platform/cache`, `internal/jobs`, `internal/search` | Cache and search test: stampede and fallback cases |

## 16. Open questions for the implementer

| # | Question | Decision owner |
| --- | --- | --- |
| Q1 | Which instance-level egress allowlist ships as the default for job and workspace networks, and what the operator UI for it looks like. | Phase 5 and phase 9 design |
| Q2 | Whether Pages builds run on the shared runner fleet or a dedicated Pages runner profile, given that Pages is not a CI surface for the site owner. | Phase 7 design |
| Q3 | Whether the webhook destination allowlist is a per-instance operator setting only or can be widened per organization. | Phase 4 design |
| Q4 | The exact retention window and export format for the audit log, and which fields are immutable under legal hold. | Phase 8 governance |
