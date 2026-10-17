# Parity checklist

**Status:** Target inventory. Every line is a GitHub capability GitNest intends to deliver and
none of it is built yet. The list exists to keep the 1:1 parity promise honest: what is written
here is the whole surface, and a gap is a visible unchecked box rather than an omission.

Each line ends with the phase that delivers it: `P1`–`P10`, mapping to the roadmap files under
`roadmap/`. A line with no phase is a decided exclusion and appears only under "Decided
exclusions".

Roadmap files: [01 identity and repository hosting](roadmap/01-identity-and-repository-hosting.md),
[02 collaboration](roadmap/02-collaboration.md), [03 pull requests](roadmap/03-pull-requests.md),
[04 API and extensibility](roadmap/04-api-and-extensibility.md), [05 Actions](roadmap/05-actions.md),
[06 packages and releases](roadmap/06-packages-and-releases.md),
[07 discovery and surfaces](roadmap/07-discovery-and-surfaces.md),
[08 governance and enterprise](roadmap/08-governance-and-enterprise.md),
[09 cloud development](roadmap/09-cloud-development.md),
[10 operations and scale](roadmap/10-operations-and-scale.md).

## Accounts and authentication — P1, P2, P4, P8

### Accounts and sign-in (P1)

- [x] Account creation by an administrator (`gitnest admin create`) — P1
- [x] Invitation-based account creation with single-use, expiring, revocable tokens — P1
- [ ] Optional self-registration controlled by an instance setting, off by default — P1
- [x] Sign in with username or email plus password — P1
- [x] Sign out that revokes the server-side session — P1
- [ ] Session list with per-session revocation and last-used information — P1
- [ ] Disable and re-enable an account, revoking its sessions — P1
- [ ] Password change with current-password confirmation — P1
- [ ] Password reset by email through a signed, expiring link — P1
- [x] Argon2id password hashing with recorded parameters — P1
- [x] Generic sign-in failures that do not disclose whether an account exists — P1
- [x] Failure-only throttling on sign-in and invitation acceptance — P1
- [ ] Email address add, verify, remove, and set-primary — P1
- [ ] Failure-only throttling on password reset requests — P1

### Sessions and browser security (P1)

- [x] Session cookies `Secure`, `HttpOnly`, `SameSite=Lax` — P1
- [x] CSRF token required on state-changing browser requests — P1
- [ ] Session expiry and idle timeout — P1
- [x] Per-route throttling wired to the shared rate limiters — P1
- [ ] Consistent session invalidations across browser, API, and Git transports — P1

### Two-factor authentication (P8)

- [ ] TOTP enrollment with QR code and an activation code requirement — P8
- [ ] TOTP verification with drift tolerance and replay rejection — P8
- [ ] WebAuthn security keys and platform authenticators, multiple per account — P8
- [ ] WebAuthn sign-counter regression rejection — P8
- [ ] Single-use recovery codes with set regeneration — P8
- [ ] Administrator recovery path requiring audited server-side action — P8
- [ ] Organization 2FA enforcement with a grace period and a non-compliance list — P8
- [ ] Enterprise 2FA enforcement policy over member organizations — P8
- [ ] Machine users exempt from interactive-only controls by account kind — P8

### SSH and GPG keys (P1, P2)

- [ ] SSH key registration with parsed type and SHA-256 fingerprint — P2
- [ ] SSH key duplicate detection, listing, and revocation — P2
- [ ] GPG key registration for commit signature display — P1
- [ ] Commit signature verification badges on commits and tags — P1
- [ ] Signed-commit enforcement as a branch protection rule — P8

### Tokens (P1, P4)

- [x] Personal access token creation showing the value once — P1
- [ ] Classic token scopes (`repo`, `read:org`, `workflow`, `packages`, and the rest) — P4
- [ ] Fine-grained tokens with a resource selector and per-resource permissions — P4
- [ ] Token expiry and last-used tracking — P4
- [ ] Single revocation path used by UI, API, and admin views — P4
- [ ] `Authorization: token` and `Authorization: Bearer` acceptance — P4

### Single sign-on and provisioning (P8)

- [ ] SAML 2.0 service provider per organization with pinned IdP certificate — P8
- [ ] SAML assertion validation for signature, audience, expiry, and replay — P8
- [ ] Just-in-time member provisioning from SAML attributes — P8
- [ ] Organization-wide SSO requirement — P8
- [ ] SCIM 2.0 users and groups with per-organization bearer token — P8
- [ ] SCIM group-to-team mapping and deactivation that revokes access — P8
- [ ] LDAP directory connection with LDAPS or StartTLS, pinned certificate validation, and failover across server URIs — P8
- [ ] LDAP user search with configurable base, filter, and attribute mapping, and password authentication by binding as the user — P8
- [ ] LDAP group search, group-to-team mapping, bounded periodic sync, and manual resync — P8
- [ ] LDAP just-in-time account creation and deactivation on directory removal, with sessions revoked — P8
- [ ] Documented precedence between local password, LDAP, SAML, and SCIM for an account that matches more than one path — P8
- [ ] Enterprise identity policies inherited by member organizations — P8

## Repositories and Git — P1, P2, P6, P7, P10

### Repository management (P1)

- [ ] Create a repository under a user or organization, private by default — P1
- [x] Repository name validation against traversal, control characters, separators, and reserved names — P1
- [ ] Describe a repository with description, website, and topics — P1
- [x] Change visibility between public and private with the required confirmation — P1
- [x] Change the default branch — P1
- [ ] Rename a repository with a redirect from the old path — P1
- [ ] Transfer a repository between owners with a target acceptance step — P2
- [x] Archive and unarchive a repository (read-only when archived) — P1
- [ ] Delete a repository as one audited operation removing metadata, storage, packages, and assets — P8
- [ ] Repository templates: create a new repository from a template — P2
- [ ] Repository language detection and display — P1
- [ ] Repository license and README detection — P1
- [ ] Watch, star, and fork counts on the repository — P7

### Permissions (P1, P2, P4)

- [x] Owner, admin, write, and read repository levels — P1
- [x] Direct collaborators at each level — P1
- [ ] Organization membership granting repository access — P2
- [ ] Team-to-repository grants with a role — P2
- [ ] Nested team inheritance with a cycle guard — P2
- [ ] Outside collaborator grants confined to granted repositories — P2
- [ ] Permission composition for tokens, apps, and installations in one decision function — P4
- [ ] Same access decision across browser, REST, GraphQL, HTTPS Git, SSH Git, and registries — P4
- [ ] Permission revocation effective on the next request — P2

### Git over HTTPS (P1)

- [x] Clone and fetch over smart HTTP with protocol v2 — P1
- [x] Push over smart HTTP with write-permission enforcement — P1
- [x] Anonymous fetch of a public repository — P1
- [x] Protocol v2 ref filtering — P1
- [x] Bounded Git process execution with timeouts, limits, cancellation, and cleanup — P1
- [x] Git LFS batch API, upload, and download addressed by content hash — P1
- [ ] Archive downloads for branches, tags, and commits in tar.gz and zip — P1

### Git over SSH (P2)

- [ ] Key-only SSH entry point with no shell, no PTY, and no forwarding — P2
- [ ] Restricted dispatch to `git-upload-pack` and `git-receive-pack` only — P2
- [ ] Repository routing from the requested SSH path with traversal rejection — P2
- [ ] Deploy keys (repository-scoped, read-only or read-write, one repository each) — P8
- [ ] SSH host key management by the operator — P2

### Repository browser (P1)

- [x] File tree browsing at a ref with directory navigation — P1
- [ ] File view with rendered Markdown, syntax highlighting, and source fallback — P1
- [ ] Raw file view and download — P1
- [ ] Blame view with commit attribution per line — P1
- [x] Commit list for a ref, filtered by path — P1
- [ ] Single commit view with diff, patch, and diff download — P1
- [ ] Compare two refs, including cross-fork comparison — P1
- [ ] Branch list and tag list with per-ref detail — P1
- [ ] Ref-scoped file finder — P1
- [ ] Line and range permalinks to a blob at a commit — P1
- [ ] Copy-permalink and copy-raw-URL affordances — P1
- [ ] Repositories list with search, filters, and sorting — P1

### Repository features and metadata (P1, P2)

- [ ] Repository topics assigned by an admin — P7
- [ ] Social preview image and repository avatar — P1
- [ ] Release, package, environment, and deployment lists on the repository page — P6
- [ ] Insights entry points (contributors, commit activity, code frequency, dependency graph, traffic) — P7
- [ ] Repository events feed — P2
- [ ] Fork list with upstream relationship — P2

## Issues — P2

### Issue lifecycle (P2)

- [x] Create an issue with title and Markdown body — P2
- [x] Per-repository shared number sequence with no gaps and no reuse — P2
- [ ] View an issue with its full timeline and metadata sidebar — P2
- [ ] Edit issue title and body with an append-only revision record — P2
- [x] Close and reopen an issue with state-change history — P2
- [x] Delete an issue with audit — P2
- [ ] Transfer an issue to another repository with a number redirect — P2
- [ ] Issue list with filters for state, label, milestone, assignee, author, and sort — P2
- [x] Issue counts that never leak a private repository's numbers — P2

### Issue metadata (P2)

- [x] Labels with name, color, and description, unique per repository after case folding — P2
- [ ] Apply and remove labels on issues and pull requests — P2
- [x] Milestones with title, description, due date, and open/closed state — P2
- [x] Milestone progress counts derived from attached issues — P2
- [ ] Multiple assignees limited to users with current read access — P2
- [ ] Issue types or a documented equivalent categorization — P2
- [ ] Pinned issues on a repository — P2
- [x] Issue templates from `.github/ISSUE_TEMPLATE` with a picker — P2
- [x] Template config that routes blank issues to a contact link — P2
- [ ] Task lists with a rendered progress summary and a toggle endpoint — P2

### Comments, mentions, and reactions (P2)

- [x] Comments with Markdown, edit history, and delete-with-audit — P2
- [x] Stable comment ordering by creation time plus identifier — P2
- [ ] `@user` mentions with notification and access-respecting rendering — P2
- [ ] `#123` and `owner/repo#123` reference resolution with a link table — P2
- [ ] Cross-references recorded in both timelines — P2
- [ ] Emoji reactions from a fixed allowlist with idempotent toggling — P2
- [x] Comment edit and delete permission rules with denied-case coverage — P2
- [x] Markdown rendering with tables, task lists, strikethrough, autolinks, and fenced code — P2
- [x] Raw HTML disabled in all rendered user text — P2

### Saved views and hygiene (P2)

- [ ] Saved issue filters per user — P2
- [ ] Bulk edit of labels, assignees, and milestone on a selection — P2
- [ ] Issue locking to restrict new comments — P2

## Pull requests and review — P3

### Pull-request lifecycle (P3)

- [ ] Open a pull request between branches in one repository — P3
- [ ] Open a pull request from a fork into an upstream repository — P3
- [ ] Draft pull requests and ready-for-review conversion — P3
- [ ] Edit title, body, base ref, and reviewers — P3
- [ ] Close, reopen, and convert to draft — P3
- [ ] Pull-request list with the same filters as issues — P3
- [ ] Linked issues with an explicit link and a documented closing-keyword set — P3
- [ ] Closing keywords that ignore code blocks and quotes — P3
- [ ] Auto-close linked issues on merge into the default branch — P3
- [ ] Reviewers and assignees on a pull request — P3
- [ ] Request a review from a user or team — P3

### Diff and comparison (P3)

- [ ] Unified and split diff views with dual line numbers and hunk headers — P3
- [ ] Per-file patches with additions and deletions counts — P3
- [ ] Rename, mode-change, binary, and submodule markers — P3
- [ ] Whitespace-only change indication — P3
- [ ] Collapse and expand context, and per-file viewed state — P3
- [ ] Server-side caps with a truncation marker and lazy file loading — P3
- [ ] File tree navigation with keyboard access — P3
- [ ] Commit-by-commit view within a pull request — P3
- [ ] Compare page with two-dot base-to-head comparison — P3
- [ ] Merge conflict detection with the conflicting path list — P3
- [ ] Local conflict-resolution guidance — P3
- [ ] Diff content escaped and never executed or injected as HTML — P3

### Review (P3)

- [ ] Review submission with comment, approve, and request-changes states — P3
- [ ] Line comments anchored to path, side, line, and diff version — P3
- [ ] Review threads with reply, edit, delete, resolve, and unresolve — P3
- [ ] Aggregate review state derived from current reviews — P3
- [ ] Outdated comment marking after a new push — P3
- [ ] Suggested changes parsed, validated, and marked stale when anchors move — P3
- [ ] Minimum approving reviewer count per repository — P3
- [ ] Requirement that the author's own approval does not count — P3
- [ ] Required review from CODEOWNERS — P8
- [ ] Dismissal of stale approvals on push with a recorded reason — P3
- [ ] Re-request review from dismissed reviewers — P3
- [ ] Required conversation resolution before merge — P8
- [ ] Self-approval rejected by policy while self-comment is allowed — P3

### Merge (P3)

- [ ] Merge commit method — P3
- [ ] Squash merge method — P3
- [ ] Rebase merge method — P3
- [ ] Allowed merge-method configuration per repository — P3
- [ ] Delete-branch-after-merge option — P3
- [ ] Mergeability evaluation combining checks, reviews, and conflicts into one explanation — P3
- [ ] Compare-and-swap ref update that a concurrent push cannot overwrite — P3
- [ ] Auto-merge intent evaluated after each head update — P3
- [ ] Per-repository merge queue with ordered, revalidated entries — P3
- [ ] Required status checks gating merge, including missing, failing, and cancelled — P8
- [ ] `strict` mode requiring the branch to be current with the base — P8
- [ ] Merge that closes exactly the linked open issues, once — P3

## Projects — P7

- [ ] Projects scoped to a repository — P7
- [ ] Projects scoped to a user — P7
- [ ] Projects scoped to an organization — P7
- [ ] Board view with columns and drag-and-drop ordering — P7
- [ ] Table view — P7
- [ ] Roadmap view — P7
- [ ] Custom fields with text, number, date, single-select, and iteration types — P7
- [ ] Field value validation by type on write — P7
- [ ] Grouping, sorting, and filtering stored per view — P7
- [ ] Items linked to issues and pull requests without duplicating the issue record — P7
- [ ] Draft items with a title and body not backed by an issue — P7
- [ ] Bounded automations (item added, field changed, item closed) with audit per run — P7
- [ ] Automation evaluation limit that prevents cascading loops — P7
- [ ] Project visibility consistent with the owning repository or organization — P7

## Discussions — P7

- [ ] Discussion categories per repository — P7
- [ ] Category formats: open discussion, question and answer, announcement — P7
- [ ] Create, edit, and close a discussion — P7
- [ ] Threaded replies — P7
- [ ] Upvotes on discussions and replies — P7
- [ ] Marking a single answer for Q&A, with re-marking audited — P7
- [ ] Polls with options and single or multiple choice voting — P7
- [ ] Poll vote uniqueness per user and a documented change cutoff — P7
- [ ] Unanswered-question filter — P7
- [ ] Discussion list with category and state filters — P7
- [ ] Discussion visibility matching issue visibility for private repositories — P7
- [ ] Category permissions enforced server-side — P7

## Wiki — P7

- [ ] Per-repository wiki backed by its own bare Git repository — P7
- [ ] Page create, edit, rename, and delete committed through the bounded Git path — P7
- [ ] Wiki page history and revision diff — P7
- [ ] Wiki sidebar navigation — P7
- [ ] Documented page-rename behaviour (redirect or inbound-link update) with a test — P7
- [ ] Concurrent edit conflict detection that loses no revision — P7
- [ ] Wiki read requires repository read, write requires repository write — P7
- [ ] Clone and edit the wiki with an ordinary Git client — P7

## Pages — P7

- [ ] Publish a static site from a configured source directory — P7
- [ ] Optional build step executed through the runner boundary — P7
- [ ] Immutable versioned artifacts with an atomic publish pointer switch — P7
- [ ] Same-origin serving under a documented path — P7
- [ ] Correct content types and no directory listing — P7
- [ ] Build log and deployment history visible to the site owner — P7
- [ ] Custom domain registration with DNS or file verification — P7
- [ ] Custom domain uniqueness with conflict rejection — P7
- [ ] TLS for custom domains in the configured mode, including proxy-terminated TLS — P7
- [ ] Pages for a private repository served only to callers with access — P7
- [ ] Failed build never serves a partial site — P7

## Releases — P6

- [ ] Release bound to a tag and an exact commit object ID — P6
- [ ] Create a tag through the API using the bounded Git runner — P6
- [ ] Release targeting an existing tag — P6
- [ ] Draft releases invisible to read-only and anonymous callers — P6
- [ ] Prerelease flag excluded from the derived `latest` — P6
- [ ] `latest` computed as newest non-draft, non-prerelease, and not user-assignable — P6
- [ ] Release notes body with rendered Markdown — P6
- [ ] Generated release notes from merged pull requests and commits, grouped by label — P6
- [ ] Contributor acknowledgement in generated notes — P6
- [ ] Release assets with upload, size cap, and content-type validation — P6
- [ ] Asset re-upload replacing rather than duplicating — P6
- [ ] Asset download with range support and `Content-Disposition` — P6
- [ ] Asset deletion as an audited operation — P6
- [ ] Per-repository release asset byte limit — P6
- [ ] Deleting a release leaves its tag unless deletion is requested explicitly — P6
- [ ] Tag move does not change the commit a release records — P6

## Packages — P6

- [ ] Package identity by registry kind, owner, name, and version — P6
- [ ] Content-addressed blob store with reference-counted versions — P6
- [ ] OCI/Docker registry: blob upload (monolithic and chunked), manifests, tags, `_catalog` — P6
- [ ] OCI authentication with token challenge — P6
- [ ] npm packument, tarball, publish, unpublish, dist-tags, and owners — P6
- [ ] npm scope mapping to the owner namespace — P6
- [ ] Maven and Gradle layout with checksums and snapshot handling — P6
- [ ] NuGet v3 service index, publish, flat container, and symbol packages — P6
- [ ] RubyGems push, yank, and compact index generation as a durable job — P6
- [ ] Composer repository with `packages.json` and dist downloads — P6
- [ ] Helm chart repository with `index.yaml` and chart push and fetch — P6
- [ ] Generic registry with versioned upload and download by name and version — P6
- [ ] Package visibility inherited from the owning repository or organization — P6
- [ ] Per-package granular access for users, teams, and installation tokens — P6
- [ ] Retention rules (newest N, newer than D days, pattern match, always keep release-referenced) — P6
- [ ] Retention dry run reporting exactly what a real run deletes — P6
- [ ] Quota accounting enforced under a lock so concurrent uploads cannot exceed the limit — P6
- [ ] Usage reporting per owner and per registry — P6
- [ ] Blob digest verification on upload and on a scheduled scrub — P6
- [ ] Package deletion participating in repository deletion — P6

## Search and discovery — P7

### Search (P7)

- [ ] Repository search over name, description, and topics — P7
- [ ] Code search over an indexed branch with language detection and path filtering — P7
- [ ] Code search result snippets with escaped content — P7
- [ ] Commit search by message, author, and date range — P7
- [ ] Issue and pull-request search — P7
- [ ] User and organization search — P7
- [ ] Topic search — P7
- [ ] Qualifier parser (`repo:`, `org:`, `user:`, `is:`, `state:`, `label:`, `milestone:`, `author:`, `assignee:`, `reviewer:`, `language:`, `topic:`, `created:`, `updated:`, `merged:`, `stars:`, `path:`, `extension:`) — P7
- [ ] Unknown qualifier rejected with a specific error — P7
- [ ] Visibility filtering before ranking, counting, and faceting — P7
- [ ] Indexing driven by durable jobs with backfill and idempotent reindex — P7
- [ ] SQL-only search baseline with no extra service required — P7
- [ ] Documented degradation with an explicit completeness signal — P7
- [ ] Indexing excludes oversized and binary files by a documented rule — P7
- [ ] Search rate limits separate from ordinary API limits — P7

### Discovery (P7)

- [ ] Topics with normalized unique slugs and topic pages — P7
- [ ] Operator-curated featured topics — P7
- [ ] Star and unstar with per-repository counts — P7
- [ ] Starred repository list per user — P7
- [ ] Watch states: participating, all activity, ignore — P7
- [ ] Follow and unfollow users with follower and following lists — P7
- [ ] Personal activity feed from followed users and watched repositories — P7
- [ ] Explore page with featured topics and active public repositories — P7
- [ ] Trending repositories from a documented rolling window — P7
- [ ] Trending recomputation that is deterministic for a fixed input set — P7

## Notifications — P2

- [ ] Notification inbox grouped by thread — P2
- [ ] Notification reasons (author, assigned, mentioned, commented, subscribed, review requested) — P2
- [ ] Per-thread subscription: participating, all activity, ignore — P2
- [ ] Mute a thread — P2
- [ ] Mark read, mark all read, and unread filtering — P2
- [ ] Notification read state that survives a process restart — P2
- [ ] Durable fan-out that produces one user-visible notification per event — P2
- [ ] Email delivery through a durable outbox with retry and bounded backoff — P2
- [ ] Per-user email settings and a digest option — P2
- [ ] Signed, expiring reply and unsubscribe links — P2
- [ ] Notification retention window — P8
- [ ] Notification fan-out respecting private repository visibility — P2

## Actions and CI — P5

### Workflow model (P5)

- [ ] Discovery of `.github/workflows/*.yml` and `*.yaml` at the triggering ref — P5
- [ ] Strict YAML parsing that rejects duplicate keys, unknown keys, and unknown step types — P5
- [ ] Published JSON Schema for workflow files — P5
- [ ] Workflow file size and job and step count caps at parse time — P5
- [ ] Run records storing the exact commit SHA the definition was read from — P5
- [ ] Disabled workflows producing no run — P5
- [ ] Triggers: `push` with branch and path filters — P5
- [ ] Triggers: `pull_request` with types, branches, and paths filters — P5
- [ ] Triggers: `schedule` (UTC cron with a minimum interval) — P5
- [ ] Triggers: `workflow_dispatch` with typed inputs — P5
- [ ] Triggers: `repository_dispatch` with a bounded JSON payload — P5
- [ ] Triggers: `release` and tag triggers — P5
- [ ] Durable run requests persisted before run creation — P5
- [ ] Duplicate event delivery producing exactly one run — P5
- [ ] Job DAG from `needs` with cycles rejected at parse time — P5
- [ ] Independent jobs running concurrently under an instance limit — P5
- [ ] Skipped dependency skipping its dependents — P5
- [ ] Failure propagation with `if: always()` — P5

### Execution model (P5)

- [ ] Expression language: contexts `github`, `env`, `vars`, `secrets`, `inputs`, `needs`, `job`, `steps`, `runner`, `matrix`, `strategy` — P5
- [ ] Expression operators and functions (`contains`, `startsWith`, `endsWith`, `format`, `join`, `toJSON`, `fromJSON`, `hashFiles`) — P5
- [ ] Status functions `success()`, `failure()`, `cancelled()`, `always()` — P5
- [ ] Bounded `hashFiles` limited to the checked-out workspace — P5
- [ ] Secret masking including base64, multi-line, and JSON-escaped forms — P5
- [ ] Matrix with `include` and `exclude` and a hard expansion cap — P5
- [ ] Matrix values validated to contain no secret material — P5
- [ ] `run` steps with explicit working directory and environment precedence — P5
- [ ] Shells: bash, sh, pwsh, and python with parse-time rejection of others — P5
- [ ] Per-step and per-job timeouts with process-tree termination — P5
- [ ] `continue-on-error` and `if` conditions — P5
- [ ] Step output capture through `GITHUB_OUTPUT` — P5
- [ ] Environment variable files `GITHUB_ENV`, `GITHUB_PATH`, and `GITHUB_STEP_SUMMARY` — P5
- [ ] JavaScript actions executed in a bundled runtime — P5
- [ ] Docker actions executed through the container adapter — P5
- [ ] Action resolution from a local path or a full-SHA-pinned remote reference — P5
- [ ] Unpinned `uses` refs rejected — P5
- [ ] Action source and resolved SHA recorded on the run — P5
- [ ] Composite actions with nested steps — P5
- [ ] Reusable workflows via `workflow_call` with inputs, outputs, and `secrets: inherit` — P5
- [ ] Bounded nesting depth with recursion rejected — P5

### Containers, caching, and logs (P5)

- [ ] Job containers with image, environment, ports, and run-scoped volumes — P5
- [ ] Service containers reachable by service name on the job network — P5
- [ ] Service health checks that fail the job with bounded logs — P5
- [ ] Deterministic container and network teardown on success, failure, timeout, and cancel — P5
- [ ] Container jobs failing explicitly when no runtime is configured — P5
- [ ] Cache with exact keys and prefix `restore-keys` — P5
- [ ] Cache branch scoping that isolates fork pull requests — P5
- [ ] Cache size caps with LRU eviction — P5
- [ ] Artifacts with a name, retention days, and a size cap — P5
- [ ] Scheduled prune job for expired artifacts and evicted caches — P5
- [ ] Logs written per step with recorded boundaries — P5
- [ ] Live log streaming with bounded memory per job — P5
- [ ] Log search within a job with step filtering — P5
- [ ] Log size cap with a visible truncation marker — P5
- [ ] Log retention tied to the run — P5
- [ ] Logs masked before storage, not only before streaming — P5

### Runs, runners, and secrets (P5)

- [ ] Cancel a running run with process-tree reaping — P5
- [ ] Rerun all jobs or failed jobs only, linked to the original run — P5
- [ ] Cancel and rerun idempotent on a terminal run — P5
- [ ] Concurrency groups with `cancel-in-progress` as a durable lease — P5
- [ ] Runner protocol (register, poll, lease, heartbeat, status, log chunk, artifact, cache) — P5
- [ ] Runner protocol versioning with additive changes and old-runner rejection — P5
- [ ] Self-hosted runners with labels and label matching — P5
- [ ] Single-use, expiring, scoped runner registration tokens — P5
- [ ] Lease expiry that returns a crashed runner's job without losing logs — P5
- [ ] Repository, organization, and environment secrets encrypted at rest — P5
- [ ] Repository, organization, and environment variables — P5
- [ ] Write-only secret rotation with no read-back endpoint — P5
- [ ] Workflow token minted per run, read-only by default, widened only by `permissions` — P5
- [ ] Fork pull requests receive no secrets and a read-only token — P5
- [ ] OIDC token issuance with a claim set bound to repository, ref, workflow, and run — P5
- [ ] Published JWKS and a configurable audience — P5
- [ ] Per-user and per-repository quotas for concurrent jobs, minutes, artifact bytes, and cache bytes — P5
- [ ] Environments with required reviewers, wait timer, and branch and tag restrictions — P5
- [ ] Pending deployment approval with an audited decision trail — P5
- [ ] Environment secrets injected only after protection passes — P5
- [ ] Expired pending deployment failing the job — P5
- [ ] Check runs keyed by head SHA with a documented authoritative-result rule — P5
- [ ] Stale results from a superseded head SHA ignored — P5
- [ ] Cancelled or skipped runs reporting that state rather than success — P5
- [ ] Job isolation: no database credentials, no host paths, no runtime socket — P5
- [ ] Egress policy with a default-deny posture and a configurable allowlist — P5

## Organizations and teams — P2, P8

### Organizations (P2)

- [ ] Create an organization — P2
- [ ] Organization profile with avatar, display name, bio, location, and links — P2
- [ ] Organization member list with search — P2
- [ ] Organization roles: owner and member — P2
- [ ] Change a member's role, refusing to leave an organization with zero owners — P2
- [ ] Remove a member, revoking organization-derived repository access — P2
- [ ] Organization-owned repositories — P2
- [ ] Organization repository list with filters — P2
- [ ] Organization projects — P7
- [ ] Organization packages list — P6
- [ ] Organization profile page listing visible teams — P7
- [ ] Organization-wide audit log view — P8
- [ ] Organization settings with inherited enterprise settings shown as inherited — P8

### Teams (P2)

- [ ] Create a team with a name and derived slug — P2
- [ ] Team description and visibility within the organization — P2
- [ ] Team members and maintainers — P2
- [ ] Nested teams with inheritance and a cycle guard — P2
- [ ] Team-to-repository grants with a role — P2
- [ ] Team review requests resolved to current members at evaluation time — P8
- [ ] Delete a team with a documented effect on its grants — P2
- [ ] Organization owners team as a managed default — P2

### Outside collaborators and machine users (P2, P4)

- [ ] Outside collaborator invitation and acceptance — P2
- [ ] Outside collaborator list per organization — P2
- [ ] Outside collaborator access confined to the granted repositories — P2
- [ ] Machine user accounts owned by an organization — P4
- [ ] Machine users unable to sign in interactively or consent to OAuth — P4

## Enterprise administration — P8, P10

- [ ] Enterprise account above organizations — P8
- [ ] Member organizations and enterprise owners — P8
- [ ] Enterprise policies: 2FA enforcement — P8
- [ ] Enterprise policies: SSO requirement — P8
- [ ] Enterprise policies: allowed repository visibilities — P8
- [ ] Enterprise policies: required rulesets — P8
- [ ] Enterprise policies: allowed app installations — P8
- [ ] Enterprise policies: token expiry and IP allow lists — P8
- [ ] Enterprise policies: organization creation policy — P8
- [ ] Explicit inheritance reporting and conflict detection per organization — P8
- [ ] Enterprise audit log with the same query and export surface as organization scope — P8
- [ ] Enterprise data export — P8
- [ ] Retention policies for audit, deliveries, job history, and notifications — P8
- [ ] Legal hold that suspends retention and deletion for a scope — P8
- [ ] Account deletion that preserves repository integrity — P8
- [ ] Repository deletion reported per subsystem as one audited operation — P8
- [ ] Moderation: abuse reporting with a disposition queue — P8
- [ ] Moderation: user blocking that hides content and prevents interaction — P8
- [ ] Moderation: temporary and permanent interaction limits — P8
- [ ] Per-account-type rate-limit tiers (interactive, machine user, installation) — P8
- [ ] Instance settings for limits reported by the operator surface — P10
- [ ] Operator surface reporting usage against configured limits — P10
- [ ] Upgrade preflight, migration locking, and rollback limits — P10
- [ ] Coordinated backup, restore drill, and disaster-recovery runbooks — P10
- [ ] Multi-node deployment with single-writer locks — P10
- [ ] Object storage, shared cache, read replicas, and external search as optional components — P10
- [ ] Performance budgets with a repeatable load suite — P10

## API and integrations — P4

### REST API (P4)

- [ ] Versioned `/api/v1` prefix with an `X-GitHub-Api-Version` header — P4
- [ ] Explicit response structs separate from domain and persistence types — P4
- [ ] `snake_case` JSON keys and RFC 3339 UTC timestamps matching GitHub — P4
- [ ] Pagination `page` starting at 1 and `per_page` defaulting to 30 and clamping at 100 — P4
- [ ] `Link` header with `next`, `prev`, `first`, and `last` relations — P4
- [ ] Filters and sorts validated against a per-endpoint allowlist and applied before counts — P4
- [ ] Strong ETags, `If-None-Match` returning `304`, and `If-Match` preconditions — P4
- [ ] `If-Modified-Since` on collection endpoints — P4
- [ ] Rate-limit headers (`X-RateLimit-Limit`, `-Remaining`, `-Reset`, `-Used`, `-Resource`) — P4
- [ ] `403` with `Retry-After` where a retry can succeed — P4
- [ ] One error body shape (`message`, `errors[]`, `documentation_url`) — P4
- [ ] Documented status code per failure class, with `404` instead of a disclosure-leaking `403` — P4
- [ ] `422` with field-level errors for validation failures — P4
- [ ] Deprecation policy with two minor releases of overlap, `Deprecation`, and `Sunset` headers — P4
- [ ] Idempotency keys on create endpoints that need them — P4
- [ ] Generated OpenAPI 3.1 document with a CI staleness check — P4
- [ ] Generated reference documentation with a runnable example per endpoint — P4
- [ ] Resource shapes for user, organization, team, repository, branch, tag, commit, tree, blob — P4
- [ ] Resource shapes for issues, comments, labels, milestones, pull requests, reviews, review comments — P4
- [ ] Resource shapes for releases, assets, check runs, check suites, workflow runs — P4
- [ ] Resource shapes for notifications, hooks, deliveries, apps, installations, token metadata — P4
- [ ] Resource shapes for packages, projects, discussions, wiki, pages, deployments, workspaces — P4

### GraphQL (P4)

- [ ] `/api/graphql` resolving against the same application services as REST — P4
- [ ] Published schema with cursor connections for lists — P4
- [ ] Query depth and complexity limits rejected before execution — P4
- [ ] Persisted queries — P4
- [ ] Introspection policy with a production default — P4
- [ ] Unreadable nodes resolving to `null` rather than an existence-disclosing error — P4
- [ ] Identical authorization decisions between REST and GraphQL — P4

### Extensibility (P4)

- [ ] OAuth app registration with hashed client secret — P4
- [ ] Authorization-code flow with required single-use `state` — P4
- [ ] PKCE for public clients — P4
- [ ] Exact-match redirect URI validation against registered callbacks — P4
- [ ] Consent screen with scope downgrade — P4
- [ ] Token exchange, refresh with rotation, and revoke — P4
- [ ] Authorized-app list with per-app and per-organization revocation — P4
- [ ] App registration with generated app ID, owner, and one-time PEM key — P4
- [ ] App authentication with a short-lived RS256 JWT — P4
- [ ] Installation creation, suspend, unsuspend, uninstall, and repository selection changes — P4
- [ ] Installation access tokens scoped, expiring within an hour, individually revocable — P4
- [ ] Installation lifecycle invalidation of outstanding tokens — P4
- [ ] Webhook event catalogue with versioned payload schemas — P4
- [ ] Repository, organization, and app webhook subscriptions — P4
- [ ] `X-Hub-Signature-256` HMAC over the raw body, with HMAC-SHA1 only for compatibility — P4
- [ ] Delivery log with request digest, response status, duration, and attempt number — P4
- [ ] Retries with exponential backoff and jitter for transport and `5xx` failures only — P4
- [ ] Manual redelivery of one delivery or a failed batch, byte-identical — P4
- [ ] Outbound webhook safety: loopback and metadata blocking, no redirects, capped response — P4
- [ ] Event payloads built by the owning domain and enqueued with the domain change — P4
- [ ] No delivery of private repository events to an unauthorized subscription — P4

## Profile and social basics — P1, P7

- [ ] User profile with avatar, display name, bio, company, location, and links — P1
- [ ] Pinned repositories filtered to what the viewer can read — P7
- [ ] Contribution activity summary — P7
- [ ] Profile repositories tab with search and filters — P7
- [ ] Profile stars tab — P7
- [ ] Followers and following lists with visibility filtering — P7
- [ ] Public activity feed per user — P7
- [ ] Account settings for profile, appearance, email, and notifications — P1
- [ ] Personal dashboard with recent activity, issues, and pull-request shortcuts — P1
- [ ] Profile for a user with only private repositories that discloses no repository names — P7
- [ ] Keyboard-usable navigation across the primary surfaces — P1
- [x] Light and dark themes — P1

## Workspaces (cloud development) — P9

### Lifecycle (P9)

- [ ] Create a workspace from a repository and ref — P9
- [ ] Create a scratch workspace with no repository — P9
- [ ] Persisted state machine over pending, building, starting, running, stopping, stopped, failed, deleting, deleted — P9
- [ ] Start, stop, rebuild, and delete a workspace — P9
- [ ] Race-safe transitions rejected by the database, not resolved in memory — P9
- [ ] Reconciliation from persisted state after a controlling-process restart — P9
- [ ] Idle reaping that stops the container and keeps the volume — P9
- [ ] Deterministic cleanup on stop, delete, failed create, timeout, and restart — P9

### Configuration and images (P9)

- [ ] Parse `.devcontainer/devcontainer.json` and root `devcontainer.json` — P9
- [ ] Support image or build context, features pinned by version, and `remoteUser` — P9
- [ ] Support `containerEnv`, `mounts`, `forwardPorts`, `workspaceFolder`, and `customizations` — P9
- [ ] Support `postCreateCommand` and `postStartCommand` with output retained on failure — P9
- [ ] Content-hash-keyed image caching that reuses only a matching configuration — P9
- [ ] Reject mounts outside GitNest-controlled roots at parse time — P9
- [ ] Prebuilds triggered by pushes to configured branches — P9
- [ ] Prebuild reuse decided by repository revision and content hash — P9
- [ ] Prebuild pruning by age and count with volume and image deletion — P9

### Runtime and access (P9)

- [ ] One volume per workspace keyed by UUID, mounted at the configured folder — P9
- [ ] Rebuild preserving the volume, with an explicit reset flag that recreates it — P9
- [ ] Disk usage measured and enforced against the workspace quota — P9
- [ ] IDE served through GitNest's own routes, never a publicly exposed workspace port — P9
- [ ] Short-lived, workspace-scoped, single-use IDE session tokens — P9
- [ ] No credential in browser local storage, page source, or the URL — P9
- [ ] Terminal over WebSocket with resize, detach, and reattach — P9
- [ ] Bounded server-side scrollback and audit records for attach and detach — P9
- [ ] Port forwarding with per-workspace preview URLs — P9
- [ ] Preview access policy: owner by default, optional share, optional signed expiring link — P9
- [ ] Preview refusal returning `404` so port existence is not disclosed — P9
- [ ] Preview route removal within a bounded time after a port stops listening — P9
- [ ] Dotfiles repository, install command, and shell preference settings — P9
- [ ] Dotfiles clone credential absent from the environment, image, and volume afterwards — P9
- [ ] Workspace shares with `read`, `edit`, and `terminal` roles — P9
- [ ] Revocation effective on the next request and bounded session termination — P9
- [ ] Multiple authorized sessions in one workspace concurrently — P9
- [ ] Snapshots with name, creator, and size, restorable into the same or a new workspace — P9
- [ ] Snapshot expiry with volume deletion and released quota accounting — P9
- [ ] Workspace-scoped and repository-scoped secrets and variables — P9
- [ ] Secrets injected at start, absent from image layers and the volume — P9
- [ ] Repository access required to create a repository-backed workspace — P9
- [ ] Repository access revocation re-evaluated for workspace access — P9
- [ ] Quotas for concurrent workspaces, CPU, memory, disk, and lifetime hours — P9
- [ ] Scheduler queueing starts at capacity with reported queue position — P9
- [ ] Isolation: non-root, dropped capabilities, no privileged mode, no host path mounts — P9
- [ ] Isolation: no container runtime socket, no configuration directory, no database credentials — P9
- [ ] Isolation: CPU, memory, pids, disk, and wall-clock limits enforced — P9
- [ ] Scratch workspace export creating a new repository from its contents — P9
- [ ] Scratch workspace never listable or reachable by another user without a share — P9

## Decided exclusions

These are deliberate non-goals. They are not planned in any phase, are not to be designed, and
must not appear as workstreams in a roadmap file.

- [ ] Mobile application — excluded; the browser and the API are the supported clients
- [ ] GitHub's own Codespaces service and its billing — excluded; GitNest delivers its own cloud-development subsystem (phase 9)
- [ ] Advanced Security: code scanning and CodeQL — excluded
- [ ] Advanced Security: secret scanning and push protection — excluded
- [ ] Advanced Security: Dependabot alerts, security updates, and dependency review — excluded
- [ ] Security advisories and vulnerability-database features — excluded
- [ ] Gist as a separate product surface — excluded; scratch workspaces replace it (phase 9)
- [ ] Sponsors, payments, billing, and paid plans — excluded
- [ ] Marketplace listing, app distribution, and Marketplace payments — excluded
- [ ] Copilot and AI-assisted features (code, review, summaries, search ranking) — excluded
- [ ] Advertising and the activity feed as a social product — excluded
- [ ] Federation between GitNest instances — excluded
- [ ] Kubernetes operators, Helm charts, and managed cloud offerings — excluded from packaging (phase 10)
- [ ] Cross-region active-active replication — excluded (phase 10)
- [ ] GitHub Enterprise Server management consoles beyond what phase 8 defines — excluded

Two clarifications that are easy to get wrong, and are therefore not exclusions:

- **GitHub Apps are in scope.** The app framework, installations, permissions, installation
  tokens, and per-app webhooks are phase 4. Only Marketplace distribution and payments are
  excluded.
- **Branch protection, required status checks, audit logs, SSO, and SCIM are in scope.** They
  are governance features (phase 8), not Advanced Security products.

## How to use this file

1. **A box is ticked only when the work exists and its acceptance criteria pass.** Work first, tick second. Ticking a box before the feature is written, or because it "mostly" works, is a false record and must be reverted. The roadmap file is authoritative for what "pass" means, including the denied-path tests, the migration, the audit event, and the updated operator documentation. A merged commit, a working demo, or a green happy-path test does not tick a box.
   - A tick must be justifiable by evidence shown in the same round: the command output, the test name, or the file that proves it.
   - An unticked box for a finished feature is as much a defect as a ticked box for a missing one. Both are corrected in the next change.
   - If a ticked capability regresses, untick it in the same change that reveals the regression and record why.
2. **An unticked box is never silently removed.** Removing a line, retagging its phase, or
   moving it to "Decided exclusions" requires an explicit decision recorded in
   [product-goals.md](product-goals.md) and the owning roadmap file in the same change. A line
   that turns out to be wrong is corrected with a note, not deleted.
3. **Ticking a box updates the phase's workstream status** in
   [implementation-plan.md](implementation-plan.md) in the same change, so the checklist and
   the plan never disagree.
4. **A new GitHub capability discovered later is added as an unticked line** with a phase tag.
   Untracked divergence between GitNest and GitHub is a defect, and the checklist is where
   divergence is made visible.
5. **A phase tag names the delivering phase, not the phase in which the line was written.**
   Where two phases share a capability (for example branch protection configuration and its
   enforcement), the line carries the later, enforcing phase.
