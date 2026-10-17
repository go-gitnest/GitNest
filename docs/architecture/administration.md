# Administration and the operator console

**Status:** Target design. Nothing in this document is implemented yet.
**Scope:** Instance administration: who administers what, which settings an operator changes at runtime, the console and CLI surfaces an operator needs, the authorization and audit obligations of each action, and the safety rules for destructive operations.
**Owner:** This document owns the operator surface. Instance settings storage and validation rules are owned by [configuration.md](configuration.md); the audit and webhook event catalogues are owned by [events.md](../reference/events.md); per-surface threats and mitigations are owned by [threat-model.md](../security/threat-model.md).

## 1. Purpose and the two layers of administration

GitNest has two independent administration layers, and conflating them is the failure this document exists to prevent.

| Layer | Scope | Who | Surfaces | Governs |
| --- | --- | --- | --- | --- |
| Instance administration | The whole deployment | Instance administrators, acting through `cmd/gitnest` or `/admin` | Operator console, CLI | Accounts, sessions, credentials, organizations (as objects), repositories (as objects), the runner fleet, delivery inspection, the audit log, instance quotas and rate limits, storage, maintenance, instance settings |
| Organization and enterprise administration | One tenant | Organization owners and admins; enterprise owners | Organization settings, enterprise settings | Membership and roles, teams, repository access, organization policy, rulesets, SSO and SCIM, organization audit views, organization quotas and retention |

Rules that keep the layers separate:

1. **An instance administrator is not a tenant administrator.** Instance administrator rights do not grant ownership of any organization, team, or repository, and do not grant read access to private repository content.
2. **A tenant administrator is not an instance administrator.** Organization and enterprise owners have no access to instance settings, the runner fleet, delivery inspection, or the instance audit ledger.
3. **Operational metadata is visible to the instance administrator without content access.** Counts, sizes, quotas, queue state, runner state, delivery state, and audit records are operator-visible. Issue bodies, pull-request diffs, file contents, action secrets, and workspace contents are not.
4. **Every instance administrative action is authorized by a shared application service, never by the console route or the React page.** The console is one caller of the same service the API uses. A UI that hides an action is a convenience, not the control.
5. **An organization can request instance-level help, but the operator decides.** Escalation paths (a stuck job, a failed delivery, a repository inconsistency) are operator actions on operational records, not tenant impersonation.

## 2. Nothing is shared between the layers

The following must never be reachable by a tenant administrator or a tenant API token, and the reverse must never be reachable by an instance administrator without a tenant grant:

| Instance-only object | Tenant-only object |
| --- | --- |
| `instance_settings` rows and their registry metadata | Organization and team membership grants |
| Runner registrations, labels, leases, and registration tokens | Repository collaborator grants and rulesets |
| Queue state, dead-letter records, job leases | Environment protection rules and approvals |
| Webhook and email delivery records across all tenants | Organization audit view and organization policy |
| The instance audit ledger and its export | Package and workspace settings owned by the tenant |
| Storage backends, storage roots, and instance-wide quota policy | Organization and repository retention policy |

An instance administrator may read a delivery record's envelope (URL host, status, attempt, duration, body digest) and must not read a delivery body that contains tenant content. Redelivery does not require body access because the body is replayed from the stored immutable payload.

## 3. Runtime settings versus values fixed at startup

The three configuration layers are defined in [configuration.md](configuration.md). This section states which layer each operator-facing value belongs to and why.

### 3.1 Fixed at startup (environment, restart required)

| Value | Why it cannot be a runtime setting |
| --- | --- |
| `GITNEST_DATABASE_URL` | Defines which database the running process is bound to; changing it at runtime means the process and the stored data disagree. |
| `GITNEST_HOST`, `GITNEST_PORT`, `GITNEST_SSH_LISTEN` | Define the listening sockets. A change requires a restart to take effect and to be reported honestly. |
| `GITNEST_PUBLIC_URL` | Defines the instance's identity: clone URLs, redirect targets, OAuth callback matching, WebAuthn relying-party ID and origin, and webhook self-target detection. A runtime change would silently invalidate every one of those. |
| `GITNEST_REPO_ROOT` | Defines where every repository path resolves. A runtime change would orphan repositories. |
| `GITNEST_SECRET_KEY` | The instance key that encrypts secrets at rest. Rotation is a deliberate operator procedure, not a setting. |
| Object storage endpoint and credentials, SMTP host and credentials, container runtime endpoint | Credentials and infrastructure endpoints belong outside the database; the database holds only the bucket or queue name and the enable/disable switch. |
| `GITNEST_LOG_LEVEL`, `GITNEST_READ_HEADER_TIMEOUT`, `GITNEST_SHUTDOWN_TIMEOUT` | Process-level values that are read once and validated as a unit. |
| Migration and schema compatibility bounds | A property of the binary, not of the data. |

### 3.2 Changed at runtime (database, no restart)

| Value | Where it lives | Change visibility |
| --- | --- | --- |
| Registration mode, allowed e-mail domains, invitation lifetime, session lifetime | `instance_settings` | Next request after the cached accessor is invalidated |
| Default repository visibility, repository creation policy | `instance_settings` | Next request |
| API rate limits and per-endpoint budgets, token lifetime policy | `instance_settings` | Next request; existing credentials keep their issued expiry |
| Runner registration policy, default job limits, artifact and cache retention, egress allowlist | `instance_settings` | Next scheduler decision |
| Instance-wide byte and count quotas per user, organization, and repository | `instance_settings` plus per-entity rows | Next quota check |
| Webhook destination allowlist, delivery attempt cap and backoff bounds | `instance_settings` | Next delivery attempt |
| Search backend selection, indexing limits | `instance_settings` | Next indexing or query |
| Two-factor enforcement, SSO and SCIM defaults, audit retention, moderation policy | `instance_settings` | Next request or next retention run |
| Workspace image allow-list, workspace quotas, idle reaping, port-forwarding policy | `instance_settings` | Next workspace decision |
| Audit retention window and legal hold state | `instance_settings` plus scope rows | Next retention run |

### 3.3 Rules for runtime settings

1. **A setting lives in exactly one layer.** Never read the same knob from both the environment and the database.
2. **A startup value may seed an instance setting once**, at migration or first boot, and the instance setting is authoritative afterwards. The console shows the seeded value and who or what seeded it.
3. **An explicitly listed startup override** (for example forcing registration closed in automation) is logged at startup and displayed in the console as overridden with the effective value. Nothing else overrides.
4. **A setting that is itself a secret is never a plaintext instance setting.** Secrets are configured at startup or stored encrypted under the instance key.
5. **Every write is validated by the registry entry before the row is written.** An invalid write changes nothing, returns a field-level error, and writes no audit event.
6. **Every accepted write is audited** as `operator.instance_setting_changed` with the key, the previous value, the new value, and the actor. Secret-bearing settings record that the value changed, never the value.
7. **A setting change that takes effect without a restart must be visible on the next request.** A cached accessor with a short time to live and explicit invalidation on write is the mechanism; a change that requires a restart is misclassified as a runtime setting and must be moved.

## 4. Operator console surfaces

The console lives under `/admin`, is reachable only to accounts holding the instance administrator role, and requires a session with a completed second factor when the account has one enrolled. Every list is paginated, filterable, and keyset-paginated for large collections; every mutation goes through a shared service.

### 4.1 Users and accounts

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| List and search accounts | Filters: status, administrator flag, created range, last activity, organization membership, repository count. Results show account fields only, never credential material. | Instance admin | `audit.record_read` for the classified detail view |
| Create an account | Operator supplies username, e-mail, and an initial credential policy (invite link or generated temporary password). Created accounts are unverified until the address is confirmed. | Instance admin | `account.created` |
| Invite an account | Generates a single-use, expiring invitation bound to the e-mail address and the intended role. | Instance admin | `account.invited` |
| Disable an account | Blocks interactive sign-in, revokes active sessions and tokens, keeps the account's data and ownership intact, and is reversible. | Instance admin | `auth.session.revoked` per session plus an account-status record |
| Enable an account | Restores sign-in. Does not restore revoked sessions, tokens, or keys. | Instance admin | account-status record |
| Reset a password | Operator triggers a reset; the account receives a single-use expiring link. The operator never sees or sets a password. Interactive password entry exists only in the local CLI, never in the console. | Instance admin | Password reset request and completion |
| Require re-enrollment after losing every factor | The administrator recovery path for an account that lost all factors: requires a recent administrative action on the server, forces re-enrollment of every factor afterwards. | Instance admin | `auth.recovery_completed` |
| Review and revoke grant authorizations | Lists OAuth and app authorizations for an account with scopes and last use; revoking is immediate. | Instance admin | `oauth_app.authorization_revoked` |
| Export an account's data | Produces a scoped archive of metadata the account owns, references to repository data, and the account's own records. | Instance admin with an explicit confirmation | `operator.data_export_created` |
| Delete an account | Multi-step, dry-run-first deletion. Anonymizes authored content where policy requires it while preserving repository integrity. | Instance admin, confirmation phrase, dry-run required | `account.deleted` |

**Impersonation policy.** GitNest does not provide account impersonation. There is no "sign in as user", no session substitution, and no break-glass path that skips audit recording. An operator who needs to see what a user sees must either hold a normal grant on the resource or be given one, and that grant is audited like any other. The rationale is that impersonation creates an unauditable content-access channel and collapses the instance/tenant separation: with impersonation, an instance administrator can read every tenant's private content, and the audit ledger cannot distinguish "administrator viewed the page" from "the account acted". The supported alternatives are:

| Need | Supported mechanism |
| --- | --- |
| Reproduce a user-visible bug | A feature-flagged diagnostic account holding ordinary grants, created and audited as an account |
| Investigate a support request about a specific resource | The tenant grants the operator an explicit role on that resource, with an audit event and a stated end time |
| Investigate authentication or session trouble | Session and authentication audit records plus per-account session listing, without reading content |
| Recover a locked-out account | The audited recovery path for factors, or a password reset by single-use link |

**Account deletion is a multi-subsystem operation.** Deletion must report a per-subsystem outcome (account row, sessions, tokens, keys, authored content, owned repositories, organization ownership, packages, workspaces, invitations) so a partial deletion is visible and repairable rather than silently assumed complete. A deletion that would leave an organization without an owner is refused until ownership is transferred; a deletion that would orphan a repository is refused until the repository is transferred or deleted.

### 4.2 Sessions and token revocation

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| List active sessions | Per account or instance-wide: session identifier prefix, creation and last-seen instants, source address and user agent, authentication method, second-factor state. Never the session token. | Instance admin | `audit.record_read` |
| Revoke one session | Takes effect on the next request in every transport. | Instance admin or the account owner | `auth.session.revoked` |
| Revoke every session for an account | Used on disable, on compromise, and on credential change. | Instance admin | `auth.session.revoked` |
| Revoke every session for a credential kind | For example every session created through a compromised SSO binding. | Instance admin | `auth.session.revoked` |
| List personal access tokens | Identifier prefix, kind (classic or fine-grained), scopes or permission set, repository selection, expiry, last-used instant, and whether it is revoked. Never the token value. | Instance admin, read-only for other accounts | `audit.record_read` |
| Revoke a token belonging to another account | Immediate; the token fails on the next request in API, Git HTTP, and SSH paths. | Instance admin | `personal_access_token.admin_revoked` |
| Revoke deployment and app credentials | Deploy keys, app private keys, and installation tokens, individually or by owner. | Instance admin | `key.deploy_key.*`, `app.private_key_revoked`, `app.uninstalled` |
| Inspect failed authentication | Failed and throttled sign-in records with source address and account, bounded by retention. | Instance admin | `auth.login.failed`, `auth.login.throttled` |
| Rotate an instance key | A deliberate procedure with a documented order and a verification step, not a console toggle. | Instance admin with a documented runbook | Key-rotation record |

### 4.3 Organization oversight

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| List organizations | Name, owner count, member count, repository count, storage usage, created instant, policy state. | Instance admin | `audit.record_read` |
| Inspect an organization | Membership and role summary, repository list, installed apps, SSO and SCIM binding state, quota usage, policy ceiling. Never the private content of its repositories. | Instance admin | `audit.record_read` |
| Rename or transfer ownership | Refused if it would leave the organization without an owner; a rename is a metadata change with a stable internal ID. | Instance admin with an explicit confirmation | Organization rename record |
| Suspend an organization | Blocks writes and new sessions for the tenant while preserving data; reversible. | Instance admin, confirmation phrase | Organization-status record |
| Delete an organization | Dry-run first, reports per-subsystem impact (repositories, packages, workspaces, teams, apps, webhooks), requires an explicit typed confirmation plus a backup within the documented window. | Instance admin | `repo.deleted` per repository plus an organization-deletion record |
| Resolve an orphaned organization | An organization whose last owner deleted their account; requires an ownership transfer or an explicit operator-appointed owner. | Instance admin | `operator.admin.role_granted` where an owner is appointed |
| Inspect enterprise policy state | Enterprise accounts, their policy inheritance, and which organizations are bound to them. | Instance admin, or enterprise owner for their own enterprise | `audit.record_read` |
| Organization-level audit views | Read-only access to organization-scoped audit records. | Instance admin or organization owner | `audit.record_read` |

### 4.4 Repository oversight

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| List and search repositories | Owner, visibility, size on disk, object count, last push, default branch, archive state, storage backend, lock state. | Instance admin | `audit.record_read` |
| Inspect repository metadata | The metadata row, storage location identifier, backend, size accounting, ref list, and recent Git operation outcomes. Never file contents or diffs. | Instance admin | `audit.record_read` on the classified detail view |
| Find repositories with operational problems | Missing directory, size accounting drift, directories not referenced by a row, a repository stuck under a stale lock, a repository whose last operation failed. | Instance admin | `audit.record_read` |
| Force-release a stale lock | Only after the stated lease or lock window has expired, with the holder and acquisition instant shown. | Instance admin, confirmation | `operator.single_writer_lock_acquired` context plus a release record |
| Rename a repository | Metadata-only operation with a stable internal ID and storage location; the old name is not silently reusable for a different repository. | Instance admin | `repo.renamed` |
| Change visibility | Public, internal, or private, with the confirmation each transition requires. | Instance admin with an explicit confirmation | `repo.visibility_changed` |
| Quarantine a repository | Makes it read-only and hidden from listings while preserving data; used for abuse response before a deletion decision. | Instance admin, confirmation | Quarantine record referencing the abuse report or incident |
| Delete a repository | Dry-run first: reports databases rows touched, storage bytes, packages, releases, assets, LFS objects, workspaces, and pages that will be removed. Requires the typed confirmation phrase and a backup within the documented window. | Instance admin | `repo.deleted` |
| Transfer ownership | Between a user and an organization or between accounts, with an audit record and an integrity check afterwards. | Instance admin | `repo.transferred` |

### 4.5 Runner fleet management and job inspection

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| List runners | Name, scope (instance, organization, repository), labels, version, status (online, idle, busy, offline), last heartbeat, jobs completed, and the supported-protocol floor. | Instance admin | `audit.record_read` |
| Inspect a runner | Labels, scope, registration instant, current lease, and recent job history. Never job secrets, environment values, or log content of another tenant. | Instance admin | `audit.record_read` |
| Register a runner scope | Create a scoped, single-use, expiring registration token for a repository, organization, or the instance. | Instance admin | `runner.registration_token_created` |
| Change runner labels | Adding or removing a label changes which jobs the runner can be matched to. | Instance admin | `runner.label_changed` |
| Pause or drain a runner | Stops new leases and lets the current job finish; used before maintenance. | Instance admin | Runner-status record |
| Remove a runner | Revokes its credentials and fails nothing retroactively; an in-flight lease is either allowed to finish or explicitly cancelled. | Instance admin, confirmation | Runner-status record |
| Triage a misbehaving runner | Disable and remove, then inspect its job history for anomalies, then decide on the affected runs. | Instance admin | Runner-status record |
| Inspect the queue | Depth by class, oldest-job age, retry counts, and dead-letter entries with their terminal error, bounded so no payload contains a secret. | Instance admin | `audit.record_read` |
| Inspect a job or run | Status, timings, step conclusions, lease holder, event trigger, and the resolved action references with their SHAs. Log content is tenant-owned: an operator sees it only with a tenant grant on that repository. | Instance admin for metadata; tenant grant for log content | `audit.record_read` |
| Cancel a stuck run | Marks non-started jobs cancelled, terminates running processes, and is idempotent (a second request on a terminal run reports the terminal state). | Instance admin | Run cancellation record referencing the workflow run |
| Rerun a failed job | Reuses the original definition; a rerun never silently observes changed secrets. | Instance admin with a repository grant | Workflow rerun record |
| Retry a dead-lettered queue job | Re-enqueues with the original idempotency key so a duplicate execution cannot double-apply. | Instance admin, confirmation | Queue-job requeue record |

### 4.6 Failed job, webhook delivery, and e-mail delivery inspection with redelivery

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| List failed webhook deliveries | Filters: subscription, event, repository or organization, response class, attempt count, date range. Columns: delivery GUID, event, target host, status, attempts, last attempt instant, duration, and the stored response-body prefix treated as untrusted text. | Instance admin, or a tenant admin for the tenant's own subscriptions | `audit.record_read` |
| Inspect a delivery | Request headers with signing material redacted, body digest and size, response status, response prefix, error, and attempt history. The signing secret and the raw payload of another tenant are never shown. | Instance admin for envelope; tenant admin for their own subscription's payload | `audit.record_read` |
| Redeliver one delivery | Replays the stored immutable payload with a new attempt record and a new delivery GUID; the original attempt is never overwritten. | Instance admin, or a tenant admin for their own subscription | `webhook.redelivery_requested` |
| Redeliver every failed delivery for a subscription | Bounded batch with a documented maximum, progress reporting, and per-delivery outcome. | Same as above | `webhook.redelivery_requested` per delivery |
| Disable a repeatedly failing subscription | Terminal failures beyond the attempt cap disable the subscription rather than retrying forever; the owner is notified. | Operator or system | Subscription-status record |
| List failed e-mail deliveries | Filters: recipient account, template or notification kind, status, attempt count, date range. E-mail bodies are rendered from domain events and are not stored with secrets. | Instance admin | `audit.record_read` |
| Redeliver an e-mail | Re-enqueues through the durable outbox with the original idempotency key so a duplicate send is detectable; a redelivery never creates a duplicate notification record. | Instance admin | E-mail redelivery record (see section 8) |
| Inspect the durable queue behind deliveries | Attempt count, next scheduled attempt, backoff state, and terminal error; no payload content beyond a bounded digest. | Instance admin | `audit.record_read` |
| Fail over the mail transport | Change the configured mail transport or sender and verify with a test send. | Instance admin | `operator.instance_setting_changed` plus a test-send record |

A delivery inspection view must answer "what failed, for whom, how many times, and when did it last try" without exposing the payload to an operator who has no grant on the tenant's content.

### 4.7 Audit log browsing and export

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| Browse the instance audit ledger | Keyset-paginated, append-only. Filters: actor, principal kind, action, target type and identifier, organization or enterprise, date range, and source address. | Instance admin | `audit.record_read` |
| Read a record's detail | The structured detail blob, with secret-bearing fields excluded by construction and by a redaction test. | Instance admin | `audit.record_read` |
| Browse organization or enterprise audit views | Scoped to the tenant's own records. | Organization or enterprise owner | `audit.record_read` |
| Export a filtered set | Produces a signed, tamper-evident file with a checksum that verification can recheck; the export records filters, record count, range, and requester. | Instance admin, or tenant owner for a tenant-scoped export | `operator.data_export_created` |
| Verify an export | Recomputes the checksum and reports any mismatch rather than silently accepting it. | Instance admin or recipient | Export verification record |
| Apply or lift a legal hold | Suspends deletion for a defined scope; the retention job refuses to delete held records. | Instance admin | `operator.legal_hold_applied`, `operator.legal_hold_lifted` |
| Configure retention | Sets the retention window per record class (audit events, webhook deliveries, job history, notification data) within an operator-visible minimum. | Instance admin | `operator.instance_setting_changed` |
| Observe a retention run | Reports how many records were deleted per class, and refuses to run when a legal hold applies. | Operator or system | `operator.retention_run_completed` |

Immutability rules the console must respect: no API path or console action may update or delete an audit record; records leave only through the retention policy or a legal hold decision; the ledger is never rewritten by a migration except by an explicit data transformation that itself records what it did.

### 4.8 Instance-wide quotas and rate-limit policy

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| Set instance quota defaults | Per-user and per-organization defaults for repository count, total bytes, package bytes, artifact and cache bytes, workspace count and hours, concurrent jobs, and job minutes. | Instance admin | `operator.instance_setting_changed` |
| Set a per-entity quota override | Raise or lower one user's, organization's, or repository's limit above the instance default. | Instance admin | `workspace.quota_changed` or the corresponding domain quota record |
| Inspect quota usage | Current usage against the effective limit per entity, with the closest-to-limit resources surfaced. | Instance admin | `audit.record_read` |
| Set rate-limit policy | Per-credential and per-source-address budgets for authenticated, unauthenticated, and app-installation callers, plus separate budgets for expensive endpoints (search, diffs, archives, registry operations). | Instance admin | `operator.instance_setting_changed` |
| Set an abuse-response tier | Moves an account between rate-limit tiers as a moderation decision; a limit on new actions, never a retroactive deletion. | Instance admin or moderator | `moderation.rate_limit_tier_changed` |
| Exempt a machine account | Machine and installation principals are distinguished by kind, never by name, so a rename cannot grant an interactive exemption. | Instance admin | `operator.instance_setting_changed` |
| Preview a policy change | Shows the entities whose effective limit or budget changes before the write. | Instance admin | `audit.record_read` |

### 4.9 Storage usage reporting

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| Report instance storage | Repository storage bytes by owner, LFS bytes, package blob bytes, artifact and cache bytes, workspace volume bytes, page build bytes, upload bytes, database size, and the free space on each configured root. | Instance admin | `audit.record_read` |
| Report per-tenant storage | The same breakdown scoped to one user, organization, or repository, joined to the quota it is measured against. | Instance admin, or tenant owner for their own tenant | `audit.record_read` |
| Report storage growth | Bytes added per day per class and the projected time to the configured limit. | Instance admin | `audit.record_read` |
| Report a storage backend mismatch | Rows referencing a backend or path that no longer exists, and directories or blobs no longer referenced by a row. | Instance admin | `audit.record_read` |
| Run the capacity report | The `gitnest doctor` style report: database size, repository storage size, object storage size, queue depth, running jobs and workspaces, and the resource closest to its limit. | Instance admin | `operator.doctor_run` |
| Sweep unreferenced blobs | Dry-run lists candidates with sizes; the write phase deletes only unreferenced, non-quarantined content older than the grace window. | Instance admin, dry-run required | Storage sweep record |
| Change the storage backend mapping | Moving LFS, artifacts, uploads, pages, or package blobs to a different backend, with a verification pass per class. | Instance admin, documented procedure | `operator.storage_backend_migrated` |

Storage reporting measures bytes, not content: it must never open a repository file, an upload, or a package blob to compute a number when a size accounting row exists.

### 4.10 Maintenance actions

| Capability | Behaviour | Authorization | Audit |
| --- | --- | --- | --- |
| Repository consistency check | Read-only pass comparing database rows to filesystem and object storage: every repository row has a directory with the expected HEAD and refs; every package version references blobs that exist with the recorded digest; every LFS pointer resolves to an object; every workspace volume referenced by a row exists; every storage directory has an owning row. Reports findings with a stable finding code and a bounded sample. | Instance admin | `audit.record_read` |
| Repository repair | Fixes only the finding classes the check can prove and that are safe to fix, each reported per action: re-create a missing directory for an empty repository, re-point a row to its existing directory, recompute a size accounting drift, remove an unreferenced empty temporary directory, quarantine a blob whose digest does not match, mark a repository read-only when its history is incomplete rather than guessing. Dry-run is mandatory; the write phase is resumable and idempotent. | Instance admin, dry-run first, confirmation | Repair record per finding and action |
| Orphan cleanup | Removes containers, networks, volumes, temporary Git roots, and staging directories left by a crash, restricted to objects carrying the expected labels and older than the grace window. | Instance admin, dry-run first | `workspace.reconciled` or the corresponding cleanup record |
| Rebuild derived data | Rebuilds the search index, a delivery or notification outbox view, or an aggregate counter from PostgreSQL, since all three are rebuildable and losing them never loses data. | Instance admin | Rebuild record |
| Migration and schema operations | Preflight (what would change), dry-run, apply, and status. Never reachable from an HTTP transport. | Instance admin, from the CLI only | `operator.schema.preflight_failed`, `operator.migration.applied`, `operator.migration.failed`, `operator.migration.lock_contended` |
| Backup and restore | Runs the documented recovery-set procedure and verifies the captured set. | Instance admin, from the CLI | `operator.backup.completed`, `operator.backup.verification_failed`, `operator.restore.completed` |
| Maintenance mode | Drains traffic to readiness failures, stops accepting new sessions and pushes, and reports the reason to users. | Instance admin | Maintenance record |

A repair must never invent history. If a repository's objects are incomplete, the correct outcome is a read-only, clearly reported state for that repository, not a plausible-looking reconstruction. Every repair finding carries the evidence that it is true (a row without a directory, a digest mismatch) so an operator can distinguish a proven defect from a heuristic.

## 5. Command-line surface

The CLI is the only path for operations that need local access, filesystem reachability, or a non-repudiable operator action. Every command below is a subcommand of `cmd/gitnest` unless stated otherwise; the runner agent is a separate binary (`cmd/gitnest-runner`) because it runs in its own trust domain.

| Command | Purpose | Authorization | Notes |
| --- | --- | --- | --- |
| `gitnest serve` | Run the web process: UI, REST and GraphQL, Git over HTTP, registries, webhook delivery. | Server-local operator | Reads startup configuration; fails clearly on an invalid value. |
| `gitnest worker` | Run the background worker: jobs, notifications, indexing, retention, delivery retries. | Server-local operator | Separate run mode so background work cannot starve request handling. |
| `gitnest ssh` | Run the SSH entry point for Git over SSH. | Server-local operator | Key-only; no shell, no PTY, no forwarding. |
| `gitnest migrate` | Apply forward-only migrations with locking and a ledger record. | Server-local operator | Never reachable from HTTP. |
| `gitnest migrate --dry-run` | Report pending migrations without applying them. | Server-local operator | The default way to inspect an upgrade. |
| `gitnest migrate --status` | Report applied and expected schema versions and the compatibility verdict. | Server-local operator | Non-zero exit when the database is newer than the binary. |
| `gitnest admin create` | Bootstrap the first administrator on an empty instance. | Local terminal only | Transactional, advisory-lock guarded, refuses a second administrator; the prompt reads the password without echo and never accepts it as an argument. |
| `gitnest admin grant <username>` | Grant the instance administrator role to an existing account. | Local terminal or console | Audited `operator.admin.role_granted`. |
| `gitnest admin revoke <username>` | Remove the instance administrator role. | Local terminal or console | Refused when it would remove the last administrator; audited `operator.admin.role_revoked`. |
| `gitnest admin password reset <account>` | Issue a single-use, expiring password-reset link or prompt for a new local password. | Local terminal (interactive reset) or console (link) | Never reveals or accepts a password from a non-interactive argument; audited. |
| `gitnest admin disable <username>` / `enable <username>` | Disable or re-enable an account, revoking active sessions on disable. | Local terminal or console | Reversible; audited. |
| `gitnest admin session revoke --user <username>` | Revoke all sessions for one account. | Local terminal or console | Audited per session. |
| `gitnest admin token revoke <token-prefix>` | Revoke a personal access token by its display prefix. | Local terminal or console | Audited `personal_access_token.admin_revoked`. |
| `gitnest admin check [--json]` | Read-only consistency check over repository, package, LFS, workspace, and storage accounting. | Local terminal or console | Reports findings; changes nothing. |
| `gitnest admin repair --finding <code> [--apply]` | Repair one proven finding class, dry-run unless `--apply`. | Local terminal, confirmation | Resumable, idempotent, audited per action. |
| `gitnest doctor` | Report usage against configured limits and name the resource closest to its limit. | Server-local operator | Read-only; safe against a production database. |
| `gitnest backup` / `gitnest restore` | Run the documented coordinated recovery-set procedure and verify it. | Server-local operator | Backup artifacts are the most sensitive data the instance holds. |
| `gitnest-runner register` | Register this runner with a scoped, single-use registration token. | Runner operator with a token minted by an instance admin | Refuses a protocol version below the supported floor. |

CLI rules:

1. **No command accepts a password, token, or secret as a command-line argument**, because arguments are visible in the process table and shell history.
2. **No command that changes data runs without an explicit confirmation or an explicit `--apply` flag.** The destructive default is dry-run.
3. **A command that needs a credential reads it from a terminal prompt, a file descriptor, or the environment — never from an argument.**
4. **Every command that mutates state writes an audit record with the operator identity available to it.** A local command records the operating-system user, the host, and the account it acted on.
5. **Migration, lock, and repair helpers are never reachable from an HTTP transport.**
6. **A command that cannot complete because the schema is unusable, the database is newer than the binary, or another node holds the migration lock fails with one specific, actionable message** rather than a generic error.

## 6. Authorization rules for console actions

| Action class | Required principal | Additional condition | Denied-case response |
| --- | --- | --- | --- |
| Read instance operational metadata (counts, sizes, queue, runners, delivery envelopes, audit records) | Instance administrator | None | Not-found for a caller without the role; the console route and the API return the same shape |
| Read tenant content (issue bodies, diffs, file contents, job logs, delivery payloads, workspace contents) | A normal grant on that resource | Instance administrator rights alone never suffice | Not-found under the disclosure rule |
| Change an instance setting | Instance administrator | The setting's registry entry is mutable and the value validates | Field-level error; nothing written |
| Create, invite, disable, enable, or delete an account | Instance administrator | Deleting requires dry-run and confirmation; a second bootstrap administrator cannot be created | Refusal naming the constraint |
| Grant or revoke the instance administrator role | Instance administrator | The last administrator cannot be removed; the change is audited | Refusal naming the constraint |
| Reset a password or run the factor-recovery path | Instance administrator | The account is not protected by an organization policy that forbids it | Refusal naming the policy |
| Revoke sessions, tokens, keys, or app credentials | Instance administrator, or the account owner for their own | None for revocation | Not-found for a caller without the role |
| Suspend, quarantine, rename, transfer, or delete a tenant object | Instance administrator | Deletion and visibility changes require the confirmation policy in section 7 | Refusal naming the missing confirmation |
| Change quotas, rate-limit policy, or retention | Instance administrator | A legal hold blocks a retention reduction for a held scope | Refusal naming the hold |
| Run a consistency check, doctor, or export | Instance administrator | None; all are read-only | Not-found for a caller without the role |
| Run a repair, cleanup, sweep, or rebuild | Instance administrator | Dry-run first; a typed confirmation for the write phase | Refusal until the dry-run is recorded |
| Migrate, back up, restore, or rotate the instance key | Instance administrator, from the CLI | A documented procedure and a maintenance window | Refusal when the schema or the lock precondition fails |
| Enterprise policy change | Enterprise owner | Cannot weaken a requirement inherited from a higher scope | Refusal naming the inherited requirement |
| Organization policy, membership, or ruleset change | Organization owner or admin | Cannot weaken an enterprise-required setting | Refusal naming the inherited requirement |

Rules that make these decisions correct in one place:

1. The decision comes from a shared application service that takes a principal and a target, not from a route guard, a React conditional, or a role name comparison in a handler.
2. The same decision governs the console and the API. A capability offered in the console has an equivalent API call guarded by the same service, or it has no API at all and is CLI-only (migration, backup, restore, key rotation).
3. Instance administrator status and organization ownership are separate grants. Holding one never implies the other.
4. Every denial on a private resource returns the same not-found shape used elsewhere, so a console or API probe cannot enumerate tenants.
5. The instance administrator role itself is a grant with an owner: it is created by the bootstrap, added by an audited action, and cannot be granted by a tenant administrator.

## 7. Safety rules for destructive actions

| Rule | Requirement |
| --- | --- |
| Classify before you build | Every console or CLI action is classified as read-only, reversible, or irreversible, and the classification is visible in the UI or the command's help. |
| Confirmation proportionate to the loss | Reversible actions take a single explicit confirmation. Irreversible actions require a typed confirmation phrase naming the object (the repository or account name), not a generic "yes". |
| Dry-run by default for anything destructive | Deletion, repair, cleanup, sweep, retention change, and quota lowering all support a dry-run that reports what would change, with counts, sizes, and a bounded sample, before the write phase. |
| Two-step for irreversible operations | The dry-run result is recorded, and the write phase requires a second, distinct invocation or an explicit `--apply`; a single click can never complete an irreversible operation. |
| Backup proximity | Repository deletion, account deletion, organization deletion, storage-backend change, and any migration that rewrites data require a verified backup within a documented window, and the console refuses the action without it. |
| Reversibility is designed, not assumed | Disable is preferred over delete, suspend and quarantine are preferred over deletion, and a deletion that must be immediate states what cannot be recovered and what the recovery point is. |
| Partial failure is reported | A multi-subsystem operation reports its outcome per subsystem and leaves a resumable record rather than reporting success for work it did not finish. |
| Retention and legal hold respected | A retention change cannot delete held records; a legal hold can be lifted only by an explicit, audited action. |
| Blast radius is visible before the write | The dry-run names the affected entity count, byte count, and the systems touched (database, repository storage, object storage, packages, workspaces, pages). |
| No silent cascade | Deleting an owner, organization, or repository refuses when it would orphan another object, and names what must be transferred or deleted first. |
| Every destructive action is attributable | The audit record names the actor, the target, the dry-run reference, the confirmation, and the per-subsystem outcome. |
| Rehearsed | Each irreversible operation is exercised in a restore drill or a documented rehearsal, and the recorded procedure is the one the console implements. |

Reversibility classification:

| Class | Examples | Required safety |
| --- | --- | --- |
| Read-only | Consistency check, doctor, audit browse, session listing, storage reporting | None beyond authorization; still audited for classified views |
| Reversible with state kept | Disable an account, suspend an organization, quarantine a repository, pause a runner, revoke a session or token | Single confirmation; the reversal path is documented next to the action |
| Reversible with effort | Rename, transfer, change visibility, change a quota, restore a backup into a clean instance | Confirmation plus a documented reversal or restore step |
| Irreversible | Delete an account, organization, or repository; sweep unreferenced blobs; apply a destructive migration; lower a retention window below existing data | Dry-run, typed confirmation, backup proximity, per-subsystem report |

## 8. What must be recorded for each action

Every record is append-only and carries actor, principal kind, action, target type and identifier, timestamp, source address and user agent where a request produced it, and a structured detail blob. Secret material is excluded by construction and by a redaction test.

| Console or CLI action | Required record |
| --- | --- |
| Create, invite, enable, disable, delete an account | `account.created`, `account.invited`, `account.deleted`, plus an account-status record for enable and disable |
| Change an account's profile or e-mail on the account's behalf | `account.updated`, `account.email_verified` |
| Grant or revoke the instance administrator role | `operator.admin.role_granted`, `operator.admin.role_revoked` |
| Bootstrap the first administrator | `operator.admin.bootstrapped` |
| Password reset request and completion | Password-reset record with the requester, the target, and the outcome; never the password or the token value |
| Factor-recovery path | `auth.recovery_completed` |
| Revoke a session, token, key, or app authorization | `auth.session.revoked`, `token.revoked`, `personal_access_token.admin_revoked`, `app.private_key_revoked`, `oauth_app.authorization_revoked` |
| Read a classified administrative view or detail | `audit.record_read`, with the view or record identifier |
| Change any instance setting | `operator.instance_setting_changed` with key, previous value, new value, and actor |
| Change a quota, including a per-entity override | `operator.instance_setting_changed` or the domain record (`workspace.quota_changed`, package retention record) |
| Change a rate-limit tier | `moderation.rate_limit_tier_changed` |
| Rename, change visibility, transfer, quarantine, or delete a repository | `repo.renamed`, `repo.visibility_changed`, `repo.transferred`, quarantine record, `repo.deleted` |
| Suspend, rename, transfer ownership of, or delete an organization | Organization-status record, ownership-transfer record, `repo.deleted` per repository |
| Register, relabel, drain, or remove a runner | `runner.registration_token_created`, `runner.label_changed`, runner-status record |
| Cancel or rerun a workflow run or job | Workflow run cancellation and rerun records referencing the run identifier |
| Requeue a dead-lettered queue job | Queue-job requeue record with the idempotency key and attempt count |
| Redeliver a webhook delivery | `webhook.redelivery_requested` per delivery |
| Disable a subscription after terminal failures | Subscription-status record |
| Redeliver an e-mail | E-mail redelivery record (event type to be added to the catalogue; see section 9) |
| Export audit records or tenant data | `operator.data_export_created`, with filters, record count, and range |
| Verify an export | Export verification record with the checksum outcome |
| Apply or lift a legal hold | `operator.legal_hold_applied`, `operator.legal_hold_lifted` |
| Run retention | `operator.retention_run_completed` with per-class deletion counts |
| Run a consistency check or doctor report | `operator.doctor_run` for the capacity report; a check record with finding codes for the consistency check |
| Run a repair, cleanup, sweep, or rebuild | Repair or cleanup record per finding and per action, referencing the dry-run that preceded it |
| Migrate the schema | `operator.migration.applied`, `operator.migration.failed`, `operator.migration.lock_contended`, `operator.schema.preflight_failed` |
| Back up or restore | `operator.backup.completed`, `operator.backup.verification_failed`, `operator.restore.completed` |
| Change the storage backend mapping | `operator.storage_backend_migrated` |
| Enter or leave maintenance mode | Maintenance record with the reason and the operator |
| Readiness degradation and recovery | `operator.readiness_degraded`, `operator.readiness_recovered` |

Three rules govern this table:

1. **The operation fails if its record cannot be written.** Losing the trail is worse than refusing the change.
2. **A record never contains a secret, a credential value, a session or token value, a private key, or tenant content.** A record may name the credential's identifier, kind, and scope.
3. **Every action in this table produces exactly one record per occurrence.** A retried action that changed nothing produces no additional record; a partially completed action reports its per-subsystem outcome.

## 9. Phase mapping

Each capability ships with the phase whose data it needs. A console action must not be offered before the surface it inspects exists, and a phase that makes a capability possible owns the console surface for it.

| Console or CLI capability | Phase | Depends on |
| --- | --- | --- |
| `gitnest serve`, `migrate`, `admin create`; bootstrap and second-administrator refusal | 0–1 | Service foundation, configuration, accounts |
| Account list, create, invite, enable, disable; session listing and revocation; failed sign-in inspection | 1 | Accounts, sessions, invitations |
| Password reset by single-use link; token listing and revocation | 1 for sessions and invitations, 4 for personal access tokens | Identity, API credentials |
| Instance settings console: registration mode, default visibility, invitation and session lifetime, local Git operation limits | 1 | `instance_settings` registry |
| Repository oversight: list, metadata, lock state, rename, visibility, quarantine; repository consistency check and repair | 1 for metadata and locks, 10 for the full check and repair | Repository metadata, storage, locks |
| Storage usage reporting per owner and per class | 1 for repository storage, 10 for the complete report | Storage accounting, `doctor` |
| Organization oversight: list, inspect, ownership repair, suspend, delete | 2 | Organizations, teams |
| Audit log browsing, filtered queries, organization audit views | 2 for the write path and basic view, 8 for query, export, retention, and legal hold | Activity, audit ledger |
| Notification and e-mail delivery inspection and redelivery | 2 | Durable outbox, mail adapter |
| SSH runner entry point and deploy-key oversight | 2 | SSH transport, keys |
| Webhook delivery inspection, retry state, and redelivery | 4 | Webhooks, delivery log |
| Runner fleet management: registration, labels, drain, removal; queue and dead-letter inspection | 5 | Runner registry, queue |
| Failed job inspection, cancellation, rerun, dead-letter requeue | 5 | Actions, runner protocol |
| Package and release oversight: retention, quotas, deletion, blob sweep | 6 | Packages, releases |
| Instance quotas and rate-limit policy console; abuse-response tiers | 4 for API limits, 6 for package quotas, 8 for moderation | Rate-limit service, quota services |
| Search index rebuild and reindex status | 7 | Search, indexing jobs |
| Pages build and custom-domain oversight; build failure inspection | 7 | Pages, runner |
| Two-factor enforcement, SSO and SCIM binding inspection, factor-recovery path | 8 | Identity, organizations |
| Data export, retention configuration, legal hold, account deletion and anonymization | 8 | Activity, retention jobs |
| Workspace fleet oversight: quotas, idle reaping, orphan cleanup, isolation-limit stops | 9 | Workspace, container runtime |
| Multi-node and lock oversight; migration preflight, dry-run, and status; storage backend migration; doctor and capacity report | 10 | Locking, migration tooling, storage backends |

## 10. Testing obligations

- Authorization: every console capability is tested with an instance administrator, an unrelated authenticated user, a tenant administrator, and an anonymous caller; the denied cases return the same not-found shape.
- Separation: an instance administrator without a repository grant cannot read repository content, job logs, or delivery payloads, tested per surface.
- Settings: an invalid write is rejected with a field-level error and changes nothing; a valid write is visible on the next request without a restart; an environment override is displayed as overridden.
- Audit: every action in the section 8 table writes exactly one record; a record cannot be updated or deleted through any API or console path; a redaction test asserts that no secret, token, session value, private key, or tenant content appears in any record.
- Deletion: a dry-run reports the same subsystem set that the write phase acts on; a partial failure is reported per subsystem and leaves a resumable record; a deletion that would orphan an object is refused with the reason.
- Impersonation: no route, parameter, header, or console action produces an authenticated session for another account; a test asserts the absence of the capability rather than trusting the documentation.
- Repair: `gitnest admin check` finds each seeded inconsistency class; `repair` fixes only the proven classes; a repeated repair is a no-op; an unrepairable repository becomes read-only and is reported rather than guessed at.
- Redelivery: a webhook redelivery creates a new attempt with a new GUID and leaves the original attempt intact; an e-mail redelivery does not create a duplicate notification; both are idempotent under a repeated request.
- CLI: no command accepts a password, token, or secret as an argument; a destructive command without `--apply` changes nothing; a command against an unmigrated schema returns its specific error.
- Integration tests that touch PostgreSQL, storage, or a container runtime are gated by `GITNEST_TEST_DATABASE_URL` and use the schema-per-test helper.

## 11. Open questions

| # | Question | Owner |
| --- | --- | --- |
| Q1 | The e-mail redelivery audit event type, and whether notification delivery inspection is an operator capability or an organization capability first. | `reference/events.md`, phase 2 |
| Q2 | Whether the consistency check runs on a schedule by default and how its findings are surfaced between runs (a console badge, an operator notification, or both). | Phase 10 |
| Q3 | The concrete grace window before an unreferenced blob or temporary directory becomes eligible for a sweep. | Phase 10 |
| Q4 | Whether instance administrators are visible to tenants in settings (a named list of administrators) and how a tenant requests operator help. | Phase 2 |
| Q5 | Whether maintenance mode is a runtime instance setting or a startup mode with its own readiness contract. | Phase 10 |
