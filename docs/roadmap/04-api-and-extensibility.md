# API and extensibility roadmap
**Phase:** 4
**Status:** not started
**Roadmap file:** docs/roadmap/04-api-and-extensibility.md

## Purpose
The versioned REST API defined in stage 1.5 covers the first-party UI. This phase turns it into a stable public contract that third parties can build on: predictable resource shapes, pagination, conditional requests, rate-limit signalling, stable errors, and a documented deprecation policy. It then adds the credential and extension models GitHub-compatible tooling expects — personal access tokens, OAuth apps, GitHub Apps, and webhooks — so CI systems, bots, and integrations can act on GitNest without a human session cookie.

The intended outcome is that an existing GitHub client library or CLI plugin can be pointed at a GitNest instance and work for the resources GitNest implements, and that when it fails, it fails with a documented, machine-readable error rather than an unexpected shape. Compatibility is a contract about the wire format, not about internal structure: GitNest keeps its modular monolith and its own storage, and no phase in this roadmap changes how a domain service decides access.

Every decision in this phase is made in shared application services under `internal/identity`, `internal/repository`, and `internal/webhooks`. `internal/transport/http/api` only parses input, delegates to a service, and maps the result to a response; it never decides whether a caller may act.

## Scope
Versioned REST surface: resource shapes, pagination, filtering, sorting, conditional requests, rate-limit headers, error bodies, API version header, deprecation policy. A GraphQL API resolving against the same application services. Credentials for automation: personal access tokens (classic and fine-grained), OAuth apps with the authorization-code flow, GitHub Apps with JWTs and installation tokens, machine users, and deploy keys. An extension and event platform: the app and token permission model, the webhook event catalogue, payload signing, a delivery log, redelivery, and retries with backoff. A machine-readable API description (OpenAPI) with generated reference documentation.

Coverage also includes the compatibility attributes clients depend on: stable JSON key names and casing, RFC 3339 UTC timestamps, clamped pagination parameters, `Link` header relations, ETag stability for an unchanged resource, and the documented status code for each failure class.

Excluded by product scope: Marketplace listing, payments, and app distribution; Copilot and AI endpoints; mobile clients. Advanced Security endpoints are not designed in any phase. The GitHub Apps framework is in scope and owned here, not by Phase 5.

## Workstreams
### W4.1 REST resource shapes and API plumbing
- **Deliverable:** A versioned route table under `internal/transport/http/api` with one response struct per public resource. The initial set is:
  - `user`, `organization`, `team`, `repository`, `collaborator`, `branch`, `tag`, `commit`, `tree`, `blob`.
  - `label`, `milestone`, `issue`, `issue_comment`, `pull_request`, `pull_request_review`, `review_comment`.
  - `release`, `release_asset`, `check_run`, `check_suite`, `workflow_run`.
  - `notification`, `hook`, `hook_delivery`, `app`, `installation`, `access_token_metadata` (metadata only; never a token value).
- One JSON encoder handles stable key order, RFC 3339 UTC timestamps, `snake_case` field names matching GitHub, and explicit `null` versus omitted-value handling. Timestamp fields carry a documented precision so a client can round-trip them.
- Response structs are separate from domain types and from persistence rows. A field is added to a response struct only deliberately; embedding a database row is not permitted.
- **Depends on:** Stage 1.5 `/api/v1` contract; `internal/identity`, `internal/repository`, `internal/organization`.
- **Acceptance:** Table-driven tests assert exact JSON for each resource against golden files. A reflection walk over every public response struct fails the build if a field name is absent from an allowlist, so a newly added secret field cannot leak by default. Timestamps round-trip identically through the encoder and decoder under a non-UTC process timezone. A struct that embeds a persistence type or a domain type fails the allowlist check.

### W4.2 Pagination, filtering, and sorting
- **Deliverable:** A shared pagination helper supporting GitHub-compatible `page`/`per_page` (default 30, maximum 100) plus opaque cursor pagination for large collections, with `Link` headers carrying `rel="next"`, `"prev"`, `"first"`, and `"last"`. Filters and sorts are validated against a per-endpoint allowlist and applied before pagination counts are computed, so private resources cannot shift a public total.
- **Depends on:** W4.1.
- **Acceptance:** Tests assert `Link` relations at the first, middle, and last page; that `per_page=1000` is clamped to 100 rather than rejected; that an out-of-range page returns an empty array with correct links; and that filtering by a resource the caller cannot read leaves the reported total unchanged compared with the same query by a privileged caller.

### W4.3 Conditional requests and ETags
- **Deliverable:** Strong ETags derived from the resource version (updated-at instant plus a content digest), `If-None-Match` returning `304` with no body, `If-Match` precondition enforcement on mutations, and `If-Modified-Since` on collection endpoints. A `304` must not carry a regenerated body or a new generation timestamp.
- **Depends on:** W4.1.
- **Acceptance:** Tests prove a second GET with the returned ETag yields `304`; a mutation with a stale `If-Match` yields `412` and leaves the row unchanged; changing an unrelated field produces a different ETag; and `304` responses are byte-empty in the response writer, not merely a body omitted by the client.

### W4.4 Rate limits and abuse signalling
- **Deliverable:** A limit service with per-credential and per-source-address buckets, differentiated limits for authenticated, unauthenticated, and app-installation callers, and response headers `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `X-RateLimit-Used`, and `X-RateLimit-Resource`. Rejections return `403` with `Retry-After` where a retry can succeed. Counting is shared across every transport that authenticates the same credential, so the API and Git transport draw on one budget.
- **Depends on:** W4.1; stage 1.2 login throttling.
- **Acceptance:** Tests cover limit exhaustion returning the documented headers and status, budget reset after the window, independent buckets for two credentials from one address, and a Git transport request decrementing the same credential bucket as an API request. A bucket survives a process restart (counter state is persisted, not in-memory only).

### W4.5 Stable errors, API version header, and deprecation policy
- **Deliverable:** One error body shape (`message`, `errors[]` with `resource`/`field`/`code`, `documentation_url`) produced by a shared error mapper that never emits internal identifiers, SQL text, or stack traces. Version negotiation via an `X-GitHub-Api-Version` header with a documented supported-version table, a path-prefix form for clients that cannot set headers, and a documented response for an unknown version. A written deprecation policy: announcement with a minimum of two minor releases of overlap, `Deprecation` and `Sunset` response headers, and a counter for deprecated-endpoint use. Validation failures return `422` with field-level errors.
- **Depends on:** W4.1.
- **Acceptance:** Tests assert the exact error body for `401`, `403`, `404`, `409`, `422`, and `429`. A request naming an unsupported version is rejected with the documented body rather than silently served the latest shape. A deprecated route emits both headers and increments a counter visible in the metrics endpoint.

### W4.6 OpenAPI description and reference generation
- **Deliverable:** A generator that emits an OpenAPI 3.1 document for the REST surface to `docs/api/openapi.json`, driven by the same route table and response structs as the handlers, plus a CI check that fails when the committed document is stale. A static reference page in `web/` rendered from that document.
- **Depends on:** W4.1, W4.5.
- **Acceptance:** `make api-docs` is deterministic — two runs are byte-identical. The CI check fails when a route is added without regeneration. A schema-lint step fails on a documented response missing a status code or on a referenced schema that does not exist.

### W4.7 GraphQL API
- **Deliverable:** A single `/api/graphql` endpoint resolving against the same application services as REST, with a published schema, cursor connections for lists, query depth and complexity limits rejected before execution, persisted queries, and a documented introspection policy (disabled by default in production). Resolvers never read database rows and never re-implement a permission check.
- **Depends on:** W4.1, W4.2.
- **Acceptance:** Tests prove a query for an unreadable private repository returns `null` for that node rather than an error that discloses existence; a query exceeding the complexity limit is rejected before any resolver runs; every schema field has a test resolving it for an authorized caller; and introspection is refused when disabled.

### W4.8 Personal access tokens (classic and fine-grained)
- **Deliverable:** A token domain in `internal/identity` with two kinds. Classic tokens carry fixed scope strings (`repo`, `read:org`, `workflow`, `packages`, `admin:public_key`, `admin:org`, and so on). Fine-grained tokens carry a resource selector (all repositories owned by the user, selected repositories, or a single owner) plus an explicit per-resource permission set. Tokens are shown once at creation, stored as a hash with a short display prefix, and carry optional expiry, last-used instant, and a single revocation path used by UI, API, and admin views. Authentication accepts `Authorization: token <t>` and `Authorization: Bearer <t>`.
- **Depends on:** Stage 1.2 identity services; W4.5.
- **Acceptance:** Tests prove the plaintext token is never persisted, returned after creation, or present in any log line; a revoked token fails on the next request across the API and Git transport; an expired token is rejected with the documented body; a fine-grained token selecting one repository is denied on another; a classic token without `repo` cannot push; and creating a token with an unknown scope fails validation.

### W4.9 OAuth apps and authorization-code flow
- **Deliverable:** An OAuth app registration model (client ID, hashed client secret, name, homepage, callback URLs, requested scopes) and an authorization-code flow with required single-use `state`, PKCE for public clients, exact-match redirect URI validation against registered callbacks, scope downgrade on the consent screen, and token exchange, refresh, and revoke endpoints. Authorized apps are listed in user settings with per-app revocation and per-organization access review.
- **Depends on:** W4.8.
- **Acceptance:** Tests cover authorization-code replay rejection, an unregistered or non-exact redirect URI being refused, a PKCE verifier mismatch, a denial at the consent step returning the documented error to the callback, refresh-token rotation invalidating the previous refresh token, and revocation taking effect on the next request.

### W4.10 GitHub Apps: registration, JWT, and installation tokens
- **Deliverable:** App registration with a generated app ID, owner (user or organization), client credentials, a PEM private key shown once, and a public app page. App authentication with a short-lived RS256 JWT signed by that key. Installation access tokens minted per installation, scoped to selected repositories, expiring within one hour and individually revocable. Installation lifecycle: install, suspend, unsuspend, uninstall, and `installation_repositories` changes. App credentials may be used only through the app-authenticated path and never as a user.
- **Depends on:** W4.8; `internal/organization`.
- **Acceptance:** Tests cover JWT expiry and signature rejection, an installation token working only on its installed repositories, an expired token requiring a fresh mint, uninstalling an app immediately invalidating outstanding tokens, and a suspended installation being refused with a documented error.

### W4.11 App and token permission model
- **Deliverable:** A permission registry mapping resource names to read/write levels, evaluated by one shared authorization service that composes user grants (owner/admin/write/read), classic token scopes, fine-grained permission sets, app installation permissions, organization policy ceilings from Phase 8, and visibility. A single decision function returns the decision plus a machine-readable reason so the UI can explain a denial. Transport layers pass a principal and never inspect a scope field.
- **Depends on:** W4.8, W4.10; stage 1.3 permission service.
- **Acceptance:** Denied-and-allowed tests exist for each principal kind against repository read, repository write, issue write, package write, and organization read. A test fails if any handler under `internal/transport/http/{api,web}` reads a scope or permission field directly. The same principal receives identical decisions through REST, GraphQL, and the Git transport for the same repository and action.

### W4.12 Webhook events and subscriptions
- **Deliverable:** A versioned event catalogue with payload schemas for `repository`, `push`, `branch`, `tag`, `issue`, `issue_comment`, `pull_request`, `pull_request_review`, `release`, `package`, `workflow_run`, `deployment_status`, `member`, `team`, `installation`, and `installation_repositories`. Subscriptions at repository, organization, and app level, each with an event list, an active flag, a content type, and a secret. Delivery is signed with `X-Hub-Signature-256` (HMAC-SHA256 over the raw body); HMAC-SHA1 is supported only for compatibility and can be disabled. Payloads are built by the domain that owns the event and enqueued from the same transaction boundary as the domain change.
- **Depends on:** W4.1; `internal/webhooks`; `internal/jobs`.
- **Acceptance:** Tests prove a signature mismatch is rejected by a sample receiver; an event for a private repository is never delivered to a subscription that lacks access to it; a committed domain change always has exactly one corresponding enqueued event payload, including after a retried transaction; and an unknown event name in a subscription is rejected at creation.

### W4.13 Webhook delivery log, redelivery, and retries
- **Deliverable:** A delivery record per attempt (subscription, event, request headers, request body digest, response status, bounded response body prefix, error, duration, attempt number, delivery GUID) retained for a documented window. Retries with exponential backoff and jitter for `5xx`, timeouts, and connection errors; no retry on `4xx`. Manual redelivery of one delivery and of a failed batch. Per-subscription delivery listing plus a repository UI to inspect, filter, and redeliver.
- **Depends on:** W4.12.
- **Acceptance:** Tests drive a stub receiver through transient failures and assert the backoff schedule, the bounded attempt count, and the terminal failure state. Redelivery recreates a byte-identical signed payload with the same delivery GUID. Stored delivery records never contain a subscription secret or a token embedded in a payload field.

### W4.14 Machine users and deploy keys
- **Deliverable:** Machine users as organization-owned accounts that cannot sign in interactively (no password, no OAuth consent, no session) and are managed like members for ownership and audit. Deploy keys as repository-scoped SSH keys with read-only or read-write access, usable by exactly one repository, with fingerprint listing, last-used tracking, and revocation. Both principals flow through the W4.11 decision function.
- **Depends on:** W4.11; `internal/transport/ssh`.
- **Acceptance:** Tests prove a machine user cannot authenticate through the browser session or OAuth consent path; a deploy key cannot read or write outside its repository even when its owner can; a revoked deploy key fails on the next SSH connection; a read-only deploy key cannot push; and adding an already-registered fingerprint to a second repository is rejected.

### W4.15 API documentation generation and content
- **Deliverable:** Human documentation generated from the OpenAPI document plus hand-written prose for authentication, pagination, conditional requests, rate limits, error semantics, versioning, deprecation, webhook verification, and app authentication. A copy-pasteable `curl` example per endpoint and a documented guarantee about JSON-to-struct fidelity for each supported version.
- **Depends on:** W4.6, W4.12.
- **Acceptance:** A test asserts every documented endpoint example has path parameter names matching the route pattern. A docs build fails when a version in the supported-version table has no page or when an endpoint in the OpenAPI document has no example. Published examples are executed against a running instance in CI for at least the authentication and repository endpoints.

## Data and interfaces

Compatibility attributes of the wire format, each of which is treated as a contract and covered by a test rather than by prose alone:

| Attribute | Committed behaviour | Version break requires |
| --- | --- | --- |
| JSON key names and casing | `snake_case`, matching GitHub for shared resources | A new API version and a deprecation window |
| Timestamps | RFC 3339 in UTC with a fixed precision | A new API version |
| Unknown response fields | Clients must ignore them; GitNest may add them within a version | Nothing |
| Pagination parameters | `page` starts at 1, `per_page` defaults to 30 and clamps to 100 | A new API version |
| `Link` header | Present on any paginated collection with more than one page | A new API version |
| ETag | Stable while the resource is unchanged, changes when it changes | A new API version |
| Error body | `message` always present; `errors[]` present for validation failures | A new API version |
| Status codes | One documented status per failure class, `404` in place of a disclosure-leaking `403` | A new API version |
| Rate-limit headers | Present on every authenticated and unauthenticated response | A new API version |

Schema additions, all forward-only migrations in `migrations/`:

- `personal_access_tokens` (user id, kind, token hash, display prefix, scopes or selector, expires at, last used at, revoked at), `fine_grained_token_permissions` (token id, resource, level, repository or owner scope).
- `oauth_apps`, `oauth_app_redirect_uris`, `oauth_authorizations` (user id, app id, scopes, organization restrictions), `oauth_authorization_codes` (single-use, PKCE challenge, expiry).
- `apps`, `app_private_keys` (encrypted PEM, one active, rotation history), `app_installations`, `installation_tokens`, `app_events`.
- `webhook_subscriptions` (owner type, owner id, target URL, hashed secret, active, event list, content type), `webhook_deliveries`, `webhook_delivery_attempts`.
- `machine_users` (user id, owning organization id), `deploy_keys` (repository id, public key, fingerprint, read-only, last used, created by).

Packages touched: `internal/identity` (tokens, OAuth apps and authorizations, GitHub Apps, machine users), `internal/repository` (deploy keys, permission composition inputs), `internal/webhooks` (subscriptions, signing, delivery, retry), `internal/activity` (audit entries for credential, app, and subscription changes), `internal/jobs` (delivery retries, token expiry sweep, delivery retention), `internal/transport/http/api` (routes, version negotiation, error mapping, ETags, rate-limit headers), `internal/transport/http/web` (user and organization settings, app pages), `internal/transport/http/git` and `internal/transport/ssh` (credential principals), and a new `internal/platform/apiversion` (version constants, header parsing, sunset bookkeeping).

API surface: versioned handlers for the resources listed in W4.1 plus search, actions, packages, and governance endpoints added by later phases as their services land; `/api/graphql`; `/login/oauth/authorize` and `/login/oauth/access_token`; `/app/installations/*`; webhook delivery endpoints; and `docs/api/openapi.json` as a generated artifact.

Events: the W4.12 catalogue is the only outbound event surface, and it is versioned alongside the API version. Credential and app lifecycle changes produce audit records, not webhooks, except for installation events.

## Security notes
- Token and client secrets are stored as hashes; the plaintext is returned once at creation and is not recoverable.
- Redirect URIs are compared exactly; `state` is required and single-use; authorization codes are short-lived and bound to client, redirect URI, and PKCE verifier.
- App JWTs are short-lived, installation tokens expire within an hour, private keys are encrypted at rest with the instance key, and key material is excluded from logs, API responses, and audit detail fields.
- Webhook delivery is an outbound request to a user-supplied URL: block loopback, link-local, and cloud metadata addresses unless an operator explicitly allowlists them; resolve then connect to prevent DNS rebinding; disable redirects; cap the response body read.
- HMAC comparison uses a constant-time comparison over the raw request body; the signed bytes are the transmitted bytes, never a re-serialized form.
- Scope and permission checks live in one shared decision function; a transport that inspects a scope field is a test failure, not a review comment.
- Rate-limit buckets are keyed by credential where one is presented, so rotating source addresses does not multiply a token's budget.
- `404` is returned instead of `403` wherever a denial would disclose the existence of a private resource.
- Every credential creation, scope change, installation, and revocation is audited with actor, credential kind, and scope — never with the secret value.
- Machine users are exempt from interactive-only controls by kind, not by name, so renaming an account cannot grant it interactive login.

## Out of scope
- Marketplace listing, billing, paid plans, and app distribution; only the app framework and installation model are in scope.
- Copilot and any AI-assisted endpoint.
- GitHub Enterprise Server-specific management consoles beyond what Phase 8 defines.
- GraphQL subscriptions over WebSocket; polling and webhooks cover the same need.
- gRPC or any non-HTTP public API surface.
- API endpoints for subsystems that do not exist yet (packages land in Phase 6; Actions and runner endpoints land in Phase 5 and register here when their services exist).

## Exit criteria
- Every public resource is reachable through both REST and GraphQL, and both paths return the same authorization decision for the same principal and action.
- The OpenAPI document is generated, committed, and CI-verified as current, with documentation built from it.
- A third-party integration can be built end to end with an OAuth app or a GitHub App: authenticate, read a repository, open an issue, receive a signed webhook, and redeliver a failed delivery.
- Personal access tokens, machine users, and deploy keys are creatable, scoped, audited, and revocable, each with denied-case tests.
- Rate limits, conditional requests, error bodies, and the version header behave as documented, proven by tests rather than by documentation alone.
