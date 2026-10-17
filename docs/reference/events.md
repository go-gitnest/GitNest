# Event catalogue

**Status:** Target design. Nothing in this document is implemented; the implementation code is being rebuilt from the documentation, and every statement here describes the intended design.
**Scope:** The three event catalogues GitNest defines — internal domain events, audit events, and webhook events — and the rules that keep them separate: naming, envelopes, versioning, delivery semantics, and what happens when an event belongs to more than one catalogue.
**Owner:** This document owns all three catalogues. `internal/activity` owns audit records and `internal/webhooks` owns webhook payloads and delivery; neither may add an event type that is not listed here.

Related documents: [../architecture.md](../architecture.md) (module boundaries, security model), [../roadmap/04-api-and-extensibility.md](../roadmap/04-api-and-extensibility.md) (W4.12 webhook events, W4.13 delivery log and retries), [../roadmap/08-governance-and-enterprise.md](../roadmap/08-governance-and-enterprise.md) (W8.7 audit log).

## 1. The three catalogues

An **event** in GitNest is not one thing. Three distinct surfaces are called events, and conflating them is the failure this document exists to prevent.

| | Internal domain event | Audit event | Webhook event |
| --- | --- | --- | --- |
| Purpose | Let one module react to another module's state change inside the process | Give an operator durable, tamper-evident evidence that a consequential change happened | Tell an external system that something happened so it can react |
| Audience | Other Go packages | Instance and organization administrators, compliance reviewers, the operator on call | Third-party services, integrations, CI systems, GitHub Apps |
| Direction | In-process, synchronous or through the durable outbox | Written to the append-only audit ledger in the same transaction as the change | Outbound HTTP POST to a subscriber's URL |
| Content | Domain identifiers and the minimum needed by the handler | Actor, principal kind, action, subject, scope, source address, user agent, structured detail | GitHub-shaped JSON payload for one event type and one action |
| Retention | Process lifetime; the outbox row is deleted once all subscribers have handled it | Long, bounded by the retention policy and legal hold; never editable through an API | Delivery records are retained for a documented window; the payload is not the record |
| Versioning | Not versioned. It is not a public contract and never crosses a process boundary. | Schema `v1`, versioned independently. A field is added, never repurposed or removed within a major version. | `X-GitHub-Hook-Version`-style payload version per event type, versioned independently of both the audit schema and the API version. |
| Failure behaviour | A failed handler fails the operation that emitted it, or is retried by the durable queue if the event was enqueued as a job | A failed audit write fails the operation. Losing the trail is worse than refusing the change. | A failed delivery is retried with bounded backoff and recorded; it never changes the outcome of the domain change |

**Rule:** these are three separate catalogues with three separate version numbers and three separate lifecycles. An audit event type is never reused as a webhook event name, a webhook payload never doubles as an audit record, and adding to one catalogue never obliges a change in another. A shared helper may transport bytes for all three; a shared *schema* for all three does not exist.

**Rule:** a webhook payload is not evidence. A consumer may drop or ignore a delivery; the audit ledger is the record that the change happened.

**Rule:** an internal domain event never carries secret material, and never crosses a trust boundary. Runners and workspace agents receive jobs and results through their own versioned protocols, not through this catalogue.

## 2. Naming conventions

### 2.1 Audit event types

`<domain>.<object>.<past_tense_verb>`, all lowercase, dot-separated, ASCII, no abbreviation invented on the spot.

| Part | Rule | Examples |
| --- | --- | --- |
| `domain` | The owning module: `account`, `session`, `token`, `key`, `org`, `team`, `repo`, `access`, `protection`, `ruleset`, `workflow`, `secret`, `app`, `package`, `workspace`, `operator` | `repo`, not `repository` for the first two segments; pick the short form and keep it |
| `object` | The thing that changed, in the singular | `visibility`, `collaborator`, `protection_rule`, `secret` |
| `verb` | Past tense, describing what happened, not what was requested | `created`, `updated`, `deleted`, `revoked`, `succeeded`, `failed`, `added`, `removed` |

Rules that keep the list usable:

- One change, one record. An operation that changes two things emits two records, each naming one subject.
- The verb describes the observable result. A rejected attempt is `.failed` or `.rejected`, never `.created` with a detail field saying it failed.
- Renaming an existing audit type is a breaking change (section 5). A new behaviour gets a new type.
- Types are stable identifiers. They are never localized, never derived from a user-facing label, and never include a name, slug, ID, or count.

### 2.2 Webhook event names

Lowercase `snake_case`, no dots, matching GitHub's event and action vocabulary so an existing integration keeps working.

| Part | Rule | Examples |
| --- | --- | --- |
| Event name | The noun in GitHub's spelling, singular where GitHub is singular | `push`, `issues`, `pull_request`, `installation_repositories` |
| Action | A `action` field inside the payload, GitHub's verbs | `opened`, `closed`, `synchronize`, `created`, `deleted` |

An event with no meaningful action (for example `push`) omits the field. A new action on an existing event is additive and does not need a new version; a new event name is a new catalogue entry.

### 2.3 Internal domain events

`<module>.<Subject><PastTense>` in Go-idiomatic form, used only inside the process, for example `IssueOpened` handled by notification fan-out and search indexing. Not part of the public contract, not listed in this document's catalogues, and never emitted directly by a transport.

## 3. Common envelope

### 3.1 Internal domain event envelope

| Field | Meaning |
| --- | --- |
| `EventID` | Process-unique identifier |
| `Type` | Internal event name |
| `OccurredAt` | Instant the change was committed |
| `ActorKind`, `ActorID` | Who caused it, or a system actor |
| `SubjectType`, `SubjectID` | The changed object, by internal ID |
| `Payload` | A typed Go value owned by the emitting module |

### 3.2 Audit event envelope

| Field | Required | Meaning |
| --- | --- | --- |
| `ID` | Yes | Stable identifier, unique per record, allocated at write time |
| `SchemaVersion` | Yes | `1` for the envelope; incremented only for a breaking envelope change |
| `Type` | Yes | Audit event type from section 2.1 |
| `OccurredAt` | Yes | RFC 3339 UTC with fixed precision, the instant the change committed |
| `ActorKind` | Yes | `user`, `machine_user`, `pat`, `oauth_app`, `github_app`, `installation`, `deploy_key`, `runner`, `system`, `operator` |
| `ActorID` | Yes | Internal ID of the acting principal; the zero ID for `system` |
| `ActorDisplay` | Yes | The login or app slug as it was at write time, snapshotted so a later rename does not rewrite history |
| `SubjectType`, `SubjectID` | Yes | The changed object and its internal ID |
| `Scope` | No | Owning organization, enterprise, repository, or workspace ID used for access-scoped queries |
| `SourceAddress`, `UserAgent` | No | Present for interactive and authenticated HTTP requests; omitted for scheduled jobs |
| `Detail` | No | Structured key-value data specific to the type. Never contains a secret, token, key, password, webhook secret, or repository content |

Audit records are append-only. No API updates or deletes one; retention and legal hold are the only removal paths, and both are enforced in one code path.

### 3.3 Webhook delivery envelope

Headers on every delivery:

| Header | Meaning |
| --- | --- |
| `X-GitHub-Event` | Event name from section 2.2 |
| `X-GitHub-Delivery` | Delivery identifier, a UUID that identifies one *delivery attempt set*, not one HTTP attempt. A redelivery reuses the identifier of the delivery it recreates. |
| `X-Hub-Signature-256` | `sha256=<hex HMAC-SHA256>` over the exact transmitted body, keyed by the subscription secret |
| `X-Hub-Signature` | `sha1=<hex HMAC-SHA1>` for compatibility only; a subscription may disable it |
| `X-GitHub-Hook-Version` | Payload version for the event type |
| `X-GitHub-Hook-Installation-Target-Type`, `...-ID` | Present when the delivery comes from an app installation |
| `Content-Type` | `application/json` (default) or `application/x-www-form-urlencoded`, per subscription setting |
| `User-Agent` | `GitNest/<version>` |

Body:

| Field | Required | Meaning |
| --- | --- | --- |
| `action` | Per event | The action, when the event defines one |
| `sender` | Yes | The acting account or app, GitHub-shaped |
| `repository` | Per event | Present for repository-scoped events |
| `organization` | Per event | Present for organization-scoped events |
| `installation` | Per installation | Present for app deliveries |
| `enterprise` | Per event | Present for enterprise-scoped events |
| Event-specific object | Yes | The payload body for the event, GitHub-shaped |

Signing rules: the signed bytes are the transmitted bytes, never a re-serialized form. Comparison is constant-time. A payload may be gzip-compressed only if the signature covers the transmitted compressed bytes; a consumer must verify before decompressing.

## 4. Audit event catalogue

Every type below is appended to the audit ledger once per occurrence, in the same transaction as the change it describes. Where the same change is also delivered to webhooks, section 7 states the rule.

### 4.1 Authentication

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `account.created` | Administrator creates an account or the instance bootstrap creates the first administrator | admin / system | account | 1 |
| `account.invited` | An invitation is created | user / admin | invitation | 1 |
| `account.invitation_accepted` | An invited account accepts and is activated | invited user | account | 1 |
| `auth.login.succeeded` | Credentials and any second factor are accepted | user | session | 1 |
| `auth.login.failed` | Credentials are rejected, including a disabled account | user / anonymous | account or unknown | 1 |
| `auth.login.throttled` | A sign-in is refused by a rate limiter | user / anonymous | source address | 1 |
| `auth.logout` | A session is ended explicitly | user | session | 1 |
| `auth.session.expired` | A session reaches its lifetime and is invalidated | system | session | 1 |
| `auth.session.revoked` | An administrator or the user revokes a session | user / admin | session | 1 |
| `auth.session.reuse_detected` | A revoked, expired, or previously rotated session token is presented | user / anonymous | session | 4 |
| `auth.2fa.enrolled` | A second factor is confirmed and becomes active | user | second factor | 8 |
| `auth.2fa.removed` | A second factor is removed by the user | user | second factor | 8 |
| `auth.2fa.verified` | A second factor is satisfied during a sign-in or a sensitive action | user | session | 8 |
| `auth.2fa.failed` | A second-factor verification fails | user | account | 8 |
| `auth.2fa.challenge_replayed` | A one-time code within its window is presented twice | user | second factor | 8 |
| `auth.totp.enrolled` / `auth.totp.removed` | TOTP enrollment completes or is removed | user | totp credential | 8 |
| `auth.webauthn.registered` / `auth.webauthn.removed` | A security key is registered or revoked | user | webauthn credential | 8 |
| `auth.webauthn.sign_count_regression` | A cloned-authenticator counter regression is detected and refused | user | webauthn credential | 8 |
| `auth.recovery_codes.generated` / `auth.recovery_codes.used` | A recovery-code set is generated, or one code is consumed | user | account | 8 |
| `auth.recovery_completed` | The administrator recovery path restores access and forces re-enrollment | admin | account | 8 |
| `auth.sso.login.succeeded` / `auth.sso.login.failed` | A SAML assertion is accepted, or rejected for signature, audience, expiry, or replay | user | sso session | 8 |
| `auth.sso.session.revoked` | An organization-scoped SSO session is invalidated | admin / IdP event | sso session | 8 |

### 4.2 Sessions, credentials, tokens, and keys

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `token.created` | A personal access token is created | user | token | 4 |
| `token.updated` | Scopes, repository selector, or expiry on a token change | user | token | 4 |
| `token.rotated` | A token is rotated and the previous value stops working | user | token | 4 |
| `token.revoked` | A token is revoked through any surface | user / admin | token | 4 |
| `token.expired` | A token reaches its expiry and stops authenticating | system | token | 4 |
| `token.used` | A token authenticates a request, recorded at a coarse interval rather than per call | token | token | 4 |
| `token.expiry_policy_changed` | An organization or enterprise changes the allowed token lifetime | admin | policy | 8 |
| `key.ssh.added` / `key.ssh.removed` | An account SSH key is added or deleted | user | ssh key | 2 |
| `key.ssh.used` | An SSH key authenticates a Git session | key | ssh key | 2 |
| `key.gpg.added` / `key.gpg.removed` | A signing key is added or removed | user | gpg key | 2 |
| `key.deploy_key.created` / `key.deploy_key.revoked` | A repository deploy key is created or revoked | admin | deploy key | 8 |
| `key.deploy_key.used` | A deploy key authenticates a Git operation | deploy key | deploy key | 8 |
| `oauth_app.created` / `oauth_app.updated` / `oauth_app.deleted` | An OAuth app registration changes | user / admin | oauth app | 4 |
| `oauth_app.secret_rotated` | A client secret is rotated | user | oauth app | 4 |
| `oauth_app.authorized` | A user authorizes an OAuth app | user | authorization | 4 |
| `oauth_app.authorization_revoked` | A user or an admin revokes an authorization | user / admin | authorization | 4 |
| `oauth_app.scopes_changed` | The granted scopes for an authorization change | user | authorization | 4 |
| `oauth_app.token_issued` / `oauth_app.token_refreshed` / `oauth_app.token_revoked` | The authorization-code flow issues, rotates, or revokes a token | oauth app | oauth token | 4 |

### 4.3 Identity lifecycle

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `account.updated` | Profile, email, or account metadata changes | user / admin | account | 1 |
| `account.email_verified` | A new address is confirmed | user | account | 1 |
| `account.disabled` / `account.enabled` | An administrator suspends or restores an account | admin | account | 1 |
| `account.deleted` | An account is deleted or anonymized under policy | admin / user | account | 8 |
| `key.recovery_code_reset` | Recovery codes are invalidated as a set | user / admin | account | 8 |
| `account.2fa_enforcement_applied` | A member is denied access because enforcement began | system | account | 8 |
| `account.password_changed` / `account.password_reset` | A password changes by the user or through recovery | user / admin | account | 1 |

### 4.4 Organization, enterprise, team, and membership

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `org.created` / `org.deleted` | An organization is created or deleted | user / owner | organization | 2 |
| `org.profile_updated` | Name, description, or contact fields change | owner | organization | 2 |
| `org.member_added` / `org.member_removed` | Organization membership changes | owner / admin | membership | 2 |
| `org.member_role_changed` | A member moves between owner, admin, and member | owner | membership | 2 |
| `org.invitation_created` / `org.invitation_revoked` | An organization invitation is issued or withdrawn | admin | invitation | 2 |
| `team.created` / `team.deleted` | A team is created or deleted | owner / admin | team | 2 |
| `team.updated` | Team name, description, or privacy changes | admin | team | 2 |
| `team.member_added` / `team.member_removed` | Team membership changes | admin | team membership | 2 |
| `team.repository_access_granted` / `team.repository_access_revoked` | A team's repository grant or role changes | admin | team grant | 2 |
| `team.parent_changed` | A team's place in the nesting hierarchy changes | admin | team | 2 |
| `org.policy_updated` | An organization policy or base permission changes | owner | policy | 8 |
| `enterprise.created` / `enterprise.deleted` | An enterprise container is created or deleted | operator / enterprise owner | enterprise | 8 |
| `enterprise.organization_added` / `enterprise.organization_removed` | An organization joins or leaves the enterprise | enterprise owner | enterprise organization | 8 |
| `enterprise.policy_updated` | An enterprise policy changes, including inherited requirements | enterprise owner | policy | 8 |
| `enterprise.member_role_changed` | Enterprise ownership or membership changes | enterprise owner | enterprise member | 8 |
| `enterprise.ldap_configured` | Directory settings are created, changed, or removed | enterprise owner / operator | directory configuration | 8 |
| `enterprise.ldap_connection_tested` | An operator runs a connection test against the directory | operator | directory configuration | 8 |
| `enterprise.ldap_sync_completed` | A directory group sync applies its changes | system | directory sync | 8 |
| `enterprise.ldap_sync_failed` | A directory sync fails and applies nothing | system | directory sync | 8 |
| `enterprise.ldap_account_provisioned` | An account is created or linked on first directory sign-in | system | account | 8 |
| `enterprise.ldap_account_deactivated` | A directory removal deactivates an account and revokes its sessions | system | account | 8 |

### 4.5 Repository lifecycle and settings

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `repo.created` | A repository is created, including from a template | user | repository | 1 |
| `repo.renamed` | The repository name changes; the internal ID and storage location do not | admin | repository | 1 |
| `repo.transferred` | Ownership moves between accounts or organizations | owner / admin | repository | 2 |
| `repo.archived` / `repo.unarchived` | The repository becomes or stops being read-only | admin | repository | 1 |
| `repo.deleted` | A repository is deleted with its packages, assets, and storage | owner / admin | repository | 8 |
| `repo.forked` | A fork is created | user | repository | 2 |
| `repo.default_branch_changed` | The default branch is changed | admin | repository | 1 |
| `repo.issues_toggled` / `repo.wiki_toggled` / `repo.projects_toggled` / `repo.discussions_toggled` | A repository feature is enabled or disabled | admin | repository | 2 |
| `repo.merge_settings_updated` | Merge method, squash, rebase, or auto-merge defaults change | admin | repository | 3 |
| `repo.action_permissions_updated` | Default workflow-token permissions or allowed actions change | admin | repository | 5 |
| `repo.pages_published` / `repo.pages_unpublished` | Pages hosting starts, changes build source, or is disabled | admin | pages site | 7 |

### 4.6 Visibility, access, and collaborator changes

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `repo.visibility_changed` | A repository moves between private, internal, and public | admin | repository | 1 |
| `repo.collaborator_added` | A direct collaborator is granted a role | admin | collaborator | 1 |
| `repo.collaborator_role_changed` | A collaborator's role changes | admin | collaborator | 1 |
| `repo.collaborator_removed` | A direct collaborator is removed | admin | collaborator | 1 |
| `repo.access.review_requested` | An access request or access review is opened | member / owner | access request | 8 |
| `repo.access.request_approved` / `repo.access.request_denied` | An access request is decided | owner | access request | 8 |
| `repo.transfer_requested` / `repo.transfer_accepted` / `repo.transfer_rejected` | A repository transfer is initiated or resolved | owner / admin | transfer request | 2 |
| `access.deploy_key_fingerprint_added` | A deploy-key fingerprint already used elsewhere is refused | admin | repository | 4 |
| `access.denied_private_repository` | A denial where the caller could otherwise infer existence (recorded without disclosing the repository to the caller) | system | repository | 8 |
| `access.permission_cache_invalidated` | A permission revocation triggers explicit cache invalidation | system | repository | 10 |

### 4.7 Protection, rulesets, and environments

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `protection.rule_created` / `protection.rule_updated` / `protection.rule_deleted` | A branch protection rule changes | admin | protection rule | 8 |
| `protection.rule_bypassed` | A push or merge proceeds through an explicit bypass grant | actor with bypass | protection rule | 8 |
| `ruleset.created` / `ruleset.updated` / `ruleset.deleted` | A repository ruleset changes | admin | ruleset | 8 |
| `ruleset.enforcement_changed` | A ruleset moves between active, evaluate, and disabled | admin | ruleset | 8 |
| `ruleset.evaluated_block` | An `evaluate` ruleset records a would-have-blocked push or merge | system | ruleset | 8 |
| `ruleset.bypass_used` | A bypass entry is exercised and named | actor with bypass | ruleset | 8 |
| `ruleset.required_by_org_enforced` | A repository owner attempts to weaken an organization-required ruleset and is refused | admin | ruleset | 8 |
| `check.required_updated` | Required status checks or app bindings on a rule change | admin | required check | 8 |
| `codeowners.error_detected` | A CODEOWNERS file fails to parse or reference a missing owner | system | repository | 8 |
| `environment.created` / `environment.updated` / `environment.deleted` | An environment or its protection rules change | admin | environment | 8 |
| `environment.secret_scope_changed` | The secret and variable scope bound to an environment changes | admin | environment | 8 |
| `deployment.review_required` | A deployment becomes pending review | system | deployment | 8 |
| `deployment.approved` / `deployment.rejected` | A required reviewer decides a deployment | reviewer | deployment | 8 |
| `deployment.rule_expired` | A pending deployment expires without a decision | system | deployment | 8 |

### 4.8 Actions, workflows, and secrets

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `workflow.run_queued` | A workflow run is created by an event or a dispatch | user / app / system | workflow run | 5 |
| `workflow.run.started` | A run leaves the queue and its first job starts | system | workflow run | 5 |
| `workflow.run.succeeded` / `workflow.run.failed` / `workflow.run.cancelled` | A run reaches a terminal conclusion | system / user | workflow run | 5 |
| `workflow.run.requested` | A workflow is dispatched through the API or a manual action | user / app | workflow run | 5 |
| `workflow.job.lease_expired` | A runner stops heartbeating and its lease is released | system | workflow job | 5 |
| `workflow.permission_ceiling_denied` | A workflow requesting permissions above repository policy is refused | system | repository | 5 |
| `secret.action_created` | A repository, organization, or environment action secret is created | admin | secret | 5 |
| `secret.action_updated` | An action secret value is replaced | admin | secret | 5 |
| `secret.action_deleted` | An action secret is deleted | admin | secret | 5 |
| `secret.actions_access_changed` | The set of workflows or repositories allowed to read a secret changes | admin | secret | 5 |
| `variable.action_changed` | A workflow variable is created, changed, or deleted | admin | variable | 5 |
| `secret.workspace_created` / `secret.workspace_rotated` / `secret.workspace_deleted` | A workspace or repository-scoped secret changes | user / admin | workspace secret | 9 |
| `runner.registration_token_created` | A scoped, single-use runner registration token is issued | admin | registration token | 5 |
| `runner.registered` / `runner.removed` | A runner registers or is removed | admin / runner | runner | 5 |
| `runner.label_changed` | The labels a runner accepts change | admin | runner | 5 |
| `runner.job_claimed` | A runner leases a job | runner | workflow job | 5 |
| `runner.rejected_version_mismatch` | A runner below the supported protocol version is refused at registration | runner | runner | 5 |
| `oidc.token_issued` | A workflow job receives an OIDC token | system | workflow job | 5 |
| `oidc.audience_policy_changed` | The allowed OIDC audiences or signing key change | admin | instance policy | 5 |
| `artifact.uploaded` / `artifact.deleted` | A build artifact is stored or removed, including by retention | system / user | artifact | 5 |
| `cache.evicted` | A cache entry is evicted by policy or quota | system | cache entry | 5 |
| `deployment.status_recorded` | A deployment attempt records a state transition | system | deployment | 8 |

### 4.9 Apps, installations, and webhook subscriptions

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `app.created` / `app.deleted` | A GitHub App registration is created or deleted | user / org owner | app | 4 |
| `app.updated` | App name, URL, callback, or webhook settings change | app owner | app | 4 |
| `app.permissions_changed` | Requested permissions on the app registration change | app owner | app | 4 |
| `app.private_key_generated` | A signing key is generated and shown once | app owner | app private key | 4 |
| `app.private_key_revoked` | A signing key is revoked; outstanding JWTs stop verifying | app owner | app private key | 4 |
| `app.installed` | An app is installed on an account or organization | user / org owner | installation | 4 |
| `app.suspended` / `app.unsuspended` | An installation is suspended or resumed | owner / admin | installation | 4 |
| `app.uninstalled` | An installation is removed and its tokens stop working | owner / admin | installation | 4 |
| `app.installation_repositories_changed` | The repository selection of an installation changes | owner / admin | installation | 4 |
| `app.token_issued` / `app.installation_token_revoked` | An installation access token is minted or revoked | app | installation token | 4 |
| `app.authorization_denied` | An installation token is used outside its installed repositories | app | installation | 4 |
| `webhook.subscription_created` / `webhook.subscription_updated` / `webhook.subscription_deleted` | A webhook subscription changes, including its event list or active flag | admin | webhook subscription | 4 |
| `webhook.secret_rotated` | A subscription signing secret is rotated | admin | webhook subscription | 4 |
| `webhook.delivery_failed` | A delivery reaches its terminal failure state | system | webhook delivery | 4 |
| `webhook.redelivery_requested` | An operator or an admin requests redelivery | user / admin | webhook delivery | 4 |
| `personal_access_token.admin_revoked` | An administrator revokes a token belonging to another account | admin | token | 8 |

### 4.10 Packages and releases

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `package.published` | A new package version is published | user / token / workflow | package version | 6 |
| `package.version_deleted` | A package version is deleted by a user or by retention | user / system | package version | 6 |
| `package.visibility_changed` | Package access moves between private, internal, and public | admin | package | 6 |
| `package.access_granted` / `package.access_revoked` | Repository or account access to a package changes | admin | package | 6 |
| `package.publish_forced_overwrite` | An authorized principal overwrites an existing version | user | package version | 6 |
| `package.retention_applied` | A retention or quota rule deletes package versions | system | package | 6 |
| `release.created` / `release.updated` / `release.deleted` | A release is created, edited, or deleted | user | release | 6 |
| `release.prerelease_toggled` | A release is marked or unmarked as a prerelease | user | release | 6 |
| `release.asset_uploaded` / `release.asset_replaced` / `release.asset_deleted` | A release asset changes | user / system | release asset | 6 |
| `registry.token_issued` | A package registry issues an authentication token | user / token | registry token | 6 |

### 4.11 Cloud development workspaces

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `workspace.created` | A workspace is created from a repository or as a scratch workspace | user | workspace | 9 |
| `workspace.started` / `workspace.stopped` / `workspace.rebuilt` | A lifecycle transition is requested and committed | user / system | workspace | 9 |
| `workspace.failed` | A build, start, or runtime step reaches a failed state | system | workspace | 9 |
| `workspace.deleted` | A workspace and its containers are removed; the volume only when requested | user | workspace | 9 |
| `workspace.idle_reaped` | The idle reaper stops a workspace | system | workspace | 9 |
| `workspace.quota_rejected` | A start or create is refused and names the exhausted resource | system | workspace | 9 |
| `workspace.quota_changed` | A per-user, per-repository, or instance quota changes | admin | quota | 9 |
| `workspace.share_granted` / `workspace.share_revoked` | A collaborator or expiring link is added or removed | owner / admin | workspace share | 9 |
| `workspace.terminal_attached` / `workspace.terminal_detached` | A terminal session opens, detaches, or ends | user | workspace session | 9 |
| `workspace.port_forwarded` | A listening port is published at a preview URL | user / system | workspace port | 9 |
| `workspace.preview_link_created` / `workspace.preview_link_expired` | A signed preview link is issued or expires | owner / system | workspace share | 9 |
| `workspace.snapshot_created` / `workspace.snapshot_restored` / `workspace.snapshot_deleted` | A snapshot is created, restored, or removed by policy | user / system | snapshot | 9 |
| `workspace.prebuild_completed` / `workspace.prebuild_pruned` | A prebuild finishes or is pruned | system | prebuild | 9 |
| `workspace.exported_to_repository` | A scratch workspace is exported into a new repository | user | repository | 9 |
| `workspace.reconciled` | A reconciler removes orphaned containers or volumes after a crash | system | workspace | 9 |
| `workspace.isolation_violation_detected` | A workspace exceeds an enforced limit and is stopped by the control plane | system | workspace | 9 |

### 4.12 Collaboration content and discovery

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `issue.created` / `issue.closed` / `issue.reopened` / `issue.deleted` | An issue changes state | user | issue | 2 |
| `issue.transferred` | An issue moves to another repository | user | issue | 2 |
| `comment.deleted` | A comment is deleted by its author or a moderator | user / moderator | comment | 2 |
| `label.created` / `label.deleted` | A label is created or deleted | admin | label | 2 |
| `milestone.closed` / `milestone.deleted` | A milestone is closed or deleted | user | milestone | 2 |
| `pull_request.merged` | A pull request is merged and refs are updated | user | pull request | 3 |
| `pull_request.merge_blocked` | A merge is refused by protection, a failing check, or a stale base | system | pull request | 8 |
| `review.dismissed` | A review is dismissed | admin / write user | review | 3 |
| `merge_queue.entry_added` / `merge_queue.entry_removed` | A pull request enters or leaves the merge queue | user / system | merge queue entry | 3 |
| `discussion.answered` / `discussion.deleted` | A discussion is answered or deleted | user / moderator | discussion | 7 |
| `poll.vote_recorded` | A poll vote is recorded | user | poll | 7 |
| `project.automation_run` | A project automation changes items | system | project | 7 |
| `wiki.page_deleted` | A wiki page is deleted | user | wiki page | 7 |
| `pages.domain_verified` / `pages.domain_removed` | A custom domain is verified or removed | admin | pages site | 7 |
| `webhook.delivery_retention_applied` | Delivery records older than the window are removed | system | webhook delivery | 8 |

### 4.13 Moderation and abuse

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `moderation.report_filed` | A report is submitted against content or an account | user | report | 8 |
| `moderation.report_dispositioned` | A report is resolved, dismissed, or escalated | moderator | report | 8 |
| `moderation.user_blocked` / `moderation.user_unblocked` | A user blocks or unblocks another account | user | block | 8 |
| `moderation.interaction_limit_set` / `moderation.interaction_limit_cleared` | A temporary or permanent interaction limit is applied or lifted | admin / moderator | interaction limit | 8 |
| `moderation.rate_limit_tier_changed` | An account moves between rate-limit tiers | admin / system | account | 8 |
| `moderation.content_removed` | Content is removed under policy | moderator | content subject | 8 |
| `audit.record_read` | An administrator reads a classified audit view or detail | admin | audit record | 8 |

### 4.14 Operator and instance actions

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `operator.admin.bootstrapped` | The first administrator is created on an empty instance | operator | account | 1 |
| `operator.migration.applied` | `gitnest migrate` applies one or more migrations | operator | schema version | 1 |
| `operator.migration.failed` | A migration fails and is rolled back | operator | schema version | 1 |
| `operator.migration.lock_contended` | A second migrate invocation waits on or is refused by the migration lock | operator | migration lock | 10 |
| `operator.schema.preflight_failed` | The preflight exits non-zero because the database is newer than the binary | operator | schema version | 10 |
| `operator.backup.completed` / `operator.backup.verification_failed` | A backup completes or fails its consistency verification | operator / system | recovery set | 10 |
| `operator.restore.completed` | A restore into a clean instance completes and verifies | operator | recovery set | 10 |
| `operator.instance_setting_changed` | An instance policy, limit, or feature setting changes | operator | instance setting | 10 |
| `operator.storage_backend_migrated` | Repository or blob storage moves to a different backend | operator | storage backend | 10 |
| `operator.retention_run_completed` | A retention job deletes records inside its policy | system | retention policy | 8 |
| `operator.legal_hold_applied` / `operator.legal_hold_lifted` | A legal hold is placed or removed for a scope | operator | legal hold | 8 |
| `operator.data_export_created` | A scoped organization or enterprise export is produced | operator / org owner | export job | 8 |
| `operator.admin.role_granted` / `operator.admin.role_revoked` | Instance administrator rights are granted or removed | operator | account | 10 |
| `operator.node_joined` / `operator.node_left` | A web node joins or leaves a multi-node deployment | operator / system | node | 10 |
| `operator.single_writer_lock_acquired` | A node takes the lock for an operation that must not run concurrently | system | lock | 10 |
| `operator.readiness_degraded` / `operator.readiness_recovered` | A dependency becomes unusable or recovers | system | dependency | 10 |
| `operator.doctor_run` | The usage and capacity report is run against an instance | operator | instance | 10 |

### 4.15 Notifications and e-mail delivery

Notification state changes that a user makes are not audited: marking a thread read is not a consequential change. Delivery outcomes are, because they affect whether an operator can tell that a user never received something.

| Event type | Trigger | Actor | Subject | Phase |
| --- | --- | --- | --- | --- |
| `notification.email_delivery_failed` | An e-mail delivery reaches its terminal failure state | system | e-mail delivery | 2 |
| `notification.email_redelivery_requested` | An operator or administrator requests redelivery of a failed e-mail from the console or CLI | operator / admin | e-mail delivery | 2 |
| `notification.email_bounce_recorded` | A permanent bounce marks an address undeliverable | system | account | 2 |
| `notification.email_suppressed` | Delivery is skipped because the address is undeliverable or the user disabled e-mail | system | account | 2 |

Redelivery is audited for the same reason webhook redelivery is: it is an operator action that causes a message to be sent to a user, and the record must show who caused it and when.

### 4.16 Audit catalogue summary

| Category | Types | First phase |
| --- | --- | --- |
| Authentication | 22 | 1 |
| Sessions, credentials, tokens, and keys | 19 | 1 |
| Identity lifecycle | 7 | 1 |
| Organization, enterprise, team, and membership | 21 | 2 |
| Repository lifecycle and settings | 11 | 1 |
| Visibility, access, and collaborators | 10 | 1 |
| Protection, rulesets, and environments | 14 | 8 |
| Actions, workflows, and secrets | 21 | 5 |
| Apps, installations, and webhook subscriptions | 16 | 4 |
| Packages and releases | 10 | 6 |
| Cloud development workspaces | 16 | 9 |
| Collaboration content and discovery | 15 | 2 |
| Moderation and abuse | 7 | 8 |
| Operator and instance actions | 17 | 1 |
| Notifications and e-mail delivery | 4 | 2 |
| **Total** | **210** | |

## 5. Webhook event catalogue

Every event below is delivered to a subscription whose event list contains the name and whose scope covers the subject. `push` is the default event for a new subscription that does not specify one.

### 5.1 Push, branches, and tags

| Event | Payload summary | Phase |
| --- | --- | --- |
| `push` | `ref`, `before`, `after`, `created`, `deleted`, `forced`, `commits[]` (id, message, timestamp, author, committer, added, removed, modified), `head_commit`, `pusher`, `repository`, `sender`; delivers one payload per push, including a branch creation or deletion with zero or one matching commits | 4 |
| `branch` | `action` = `created` or `deleted`, `ref`, `ref_type`, `master_branch`, `repository`, `sender` | 4 |
| `tag` | `action` = `created` or `deleted`, `ref`, `ref_type`, `repository`, `sender` | 4 |

### 5.2 Issues and comments

| Event | Payload summary | Phase |
| --- | --- | --- |
| `issues` | `action` = `opened`, `edited`, `deleted`, `transferred`, `pinned`, `unpinned`, `closed`, `reopened`, `assigned`, `unassigned`, `labeled`, `unlabeled`, `locked`, `unlocked`, `milestoned`, `demilestoned`; `issue` (number, state, title, body, user, labels, assignees, milestone), `changes` for edited fields, `repository`, `sender` | 4 |
| `issue_comment` | `action` = `created`, `edited`, `deleted`; `issue`, `comment` (id, body, user, created_at, updated_at), `changes`, `repository`, `sender` | 4 |
| `issues` `labeled`/`unlabeled` | Carries `label` and `changes` in addition to the issue body | 2 |
| `milestone` | `action` = `created`, `closed`, `opened`, `edited`, `deleted`; `milestone` (number, title, state, due_on), `changes`, `repository`, `sender` | 2 |
| `label` | `action` = `created`, `edited`, `deleted`; `label` (name, color, description), `changes`, `repository`, `sender` | 2 |

### 5.3 Pull requests and review

| Event | Payload summary | Phase |
| --- | --- | --- |
| `pull_request` | `action` = `opened`, `edited`, `closed`, `reopened`, `synchronize`, `assigned`, `unassigned`, `labeled`, `unlabeled`, `review_requested`, `review_request_removed`, `ready_for_review`, `converted_to_draft`, `locked`, `unlocked`, `auto_merge_enabled`, `auto_merge_disabled`, `milestoned`, `demilestoned`; `number`, `pull_request` (base and head refs with object IDs, draft, merged, mergeable_state), `changes`, `repository`, `sender` | 4 |
| `pull_request` `closed` with `merged: true` | Carries `merge_commit_sha`, the merged-by user, and the resolved merge method | 3 |
| `pull_request_review` | `action` = `submitted`, `edited`, `dismissed`; `review` (id, state, body, user, submitted_at), `pull_request`, `changes`, `repository`, `sender` | 4 |
| `pull_request_review_comment` | `action` = `created`, `edited`, `deleted`; `comment` (id, path, position, line, side, body, in_reply_to_id, commit_id, user), `pull_request`, `changes`, `repository`, `sender` | 4 |
| `pull_request_review_thread` | `action` = `resolved`, `unresolved`; `thread` (id, resolved, path), `pull_request`, `repository`, `sender` | 3 |
| `merge_group` | `action` = `checks_requested`, `destroyed`; `merge_group` (head_sha, head_ref, base_ref), `repository`, `sender` | 3 |
| `check_run` | `action` = `created`, `completed`, `rerequested`, `requested_action`; `check_run` (id, name, status, conclusion, started_at, completed_at, output), `requested_action`, `repository`, `sender` | 3 |
| `check_suite` | `action` = `completed`, `requested`, `rerequested`; `check_suite` (id, head_sha, status, conclusion, pull_requests[]), `repository`, `sender` | 3 |

### 5.4 Releases and packages

| Event | Payload summary | Phase |
| --- | --- | --- |
| `release` | `action` = `published`, `unpublished`, `created`, `edited`, `deleted`, `prereleased`, `released`; `release` (id, tag_name, name, draft, prerelease, target_commitish, assets[]), `changes`, `repository`, `sender` | 6 |
| `package` | `action` = `published`, `updated`; `package` (name, package_type, version, registry, visibility, owner), `repository`, `sender` | 6 |
| `registry_package` (compatibility name) | Same body as `package`, delivered to subscriptions that request the GitHub-compatible name | 6 |
| `package_version_deleted` | `action` = `deleted` or `retention_deleted`; `package`, `version`, `actor` (user or scheduled job) | 6 |

### 5.5 Workflow runs, jobs, and deployments

| Event | Payload summary | Phase |
| --- | --- | --- |
| `workflow_run` | `action` = `requested`, `in_progress`, `completed`; `workflow_run` (id, name, run_number, event, head_branch, head_sha, status, conclusion, run_attempt, actor, triggering_actor, pull_requests[], started_at, updated_at), `workflow` (id, path, name), `repository`, `sender` | 5 |
| `workflow_job` | `action` = `queued`, `in_progress`, `completed`, `waiting`; `workflow_job` (id, run_id, name, labels[], runner_id, status, conclusion, started_at, completed_at, steps[] with name, status, conclusion, number), `repository`, `sender` | 5 |
| `workflow_dispatch` | `action` = `requested`; `inputs`, `ref`, `workflow`, `repository`, `sender` | 5 |
| `deployment` | `action` = `created`; `deployment` (id, ref, sha, environment, description, transient_environment, production_environment, creator), `workflow_run`, `repository`, `sender` | 8 |
| `deployment_status` | `action` = `created`; `deployment_status` (id, state = `pending`, `approved`, `rejected`, `in_progress`, `success`, `failure`, `cancelled`, description, environment, creator), `deployment`, `check_run`, `repository`, `sender` | 5 |
| `deployment_review` | `action` = `requested`, `approved`, `rejected`; `deployment`, `review` (id, state, user, comment), `workflow_run`, `repository`, `sender` | 8 |

### 5.6 Repositories

| Event | Payload summary | Phase |
| --- | --- | --- |
| `repository` | `action` = `created`, `deleted`, `archived`, `unarchived`, `publicized`, `privatized`, `renamed`, `edited`, `transferred`, `edited` (default branch), `edited` (topics); `repository`, `changes`, `organization`, `sender` | 4 |
| `repository_vulnerability_alert` | Not delivered. Advanced Security is out of scope and no payload exists. | — |
| `fork` | `forkee` (the new repository), `repository`, `sender` | 2 |
| `repository_import` | `action` = `created`; `status`, `repository`, `sender` | 2 |
| `create` | `ref`, `ref_type` = `repository`, `branch`, or `tag`, `master_branch`, `description`, `repository`, `sender` | 2 |
| `delete` | `ref`, `ref_type` = `branch` or `tag`, `pusher_type`, `repository`, `sender` | 2 |
| `public` | `repository` becomes public, `sender` | 4 |
| `star` | `action` = `created`, `deleted`; `starred_at`, `repository`, `sender` | 7 |
| `watch` | `action` = `started`; `repository`, `sender` | 7 |

### 5.7 Organizations, members, and teams

| Event | Payload summary | Phase |
| --- | --- | --- |
| `organization` | `action` = `created`, `deleted`, `renamed`, `member_added`, `member_removed`, `member_invited`; `organization`, `membership`, `invitation`, `changes`, `sender` | 4 |
| `org_block` | `action` = `blocked`, `unblocked`; `organization`, `blocked_user`, `sender` | 8 |
| `member` | `action` = `added`, `edited`, `removed`; `member` (login, id), `changes` (role or permission), `organization`, `repository`, `scope`, `sender` | 4 |
| `membership` | `action` = `added`, `removed`; `scope` = `team`; `member`, `team`, `organization`, `sender` | 4 |
| `team` | `action` = `created`, `deleted`, `edited`, `added_to_repository`, `removed_from_repository`; `team` (id, name, permission, privacy, parent), `repository`, `changes`, `organization`, `sender` | 4 |
| `team_add` | A repository is added to a team; `team`, `repository`, `organization`, `sender` | 4 |
| `repository` `transferred` | Carries `changes.owner` and the new owner | 2 |
| `custom_property_values` | `action` = `created`, `updated`, `deleted`; `custom_property`, `repository`, `organization`, `sender` | 8 |

### 5.8 Apps, installations, and access

| Event | Payload summary | Phase |
| --- | --- | --- |
| `installation` | `action` = `created`, `deleted`, `suspend`, `unsuspend`, `new_permissions_accepted`; `installation` (id, account, repository_selection, permissions, events), `repositories[]`, `requester`, `sender` | 4 |
| `installation_repositories` | `action` = `added`, `removed`; `installation`, `repositories_added[]`, `repositories_removed[]`, `repository_selection`, `sender` | 4 |
| `github_app_authorization` | `action` = `revoked`; `sender` | 4 |
| `deploy_key` | `action` = `created`, `deleted`; `key` (id, title, fingerprint, read_only, created_at), `repository`, `sender` | 8 |

### 5.9 Discussions and projects

| Event | Payload summary | Phase |
| --- | --- | --- |
| `discussion` | `action` = `created`, `edited`, `deleted`, `transferred`, `pinned`, `unpinned`, `labeled`, `unlabeled`, `locked`, `unlocked`, `category_changed`, `answered`, `unanswered`; `discussion` (number, title, body, category, answer, state), `changes`, `repository`, `sender` | 7 |
| `discussion_comment` | `action` = `created`, `edited`, `deleted`; `discussion`, `comment` (id, body, parent_id, user) | 7 |
| `projects_v2` | `action` = `created`, `edited`, `deleted`, `closed`, `reopened`; `projects_v2` (id, title, state, fields) | 7 |
| `projects_v2_item` | `action` = `created`, `edited`, `deleted`, `archived`, `restored`, `converted`; `projects_v2_item` (id, content_type, content_node_id), `changes` for field values | 7 |

### 5.10 Webhook catalogue summary

| Category | Events | First phase |
| --- | --- | --- |
| Push, branches, and tags | 3 | 4 |
| Issues and comments | 5 | 2 |
| Pull requests and review | 8 | 3 |
| Releases and packages | 4 | 6 |
| Workflow runs, jobs, and deployments | 6 | 5 |
| Repositories | 9 | 2 |
| Organizations, members, and teams | 9 | 2 |
| Apps, installations, and access | 4 | 4 |
| Discussions and projects | 4 | 7 |
| **Total** | **52** | |

## 6. Payload versioning

Three version numbers exist and move independently:

| Surface | Version carrier | Rule |
| --- | --- | --- |
| Audit schema | `SchemaVersion` in each record | Incremented only for a breaking envelope change. Field additions within a version are free. An audit type is never renamed in place; a rename means a new type, and the old type stops being written. |
| Webhook payload | `X-GitHub-Hook-Version` request header, plus the event name and action | Additive changes (a new field, a new action, a new event name) do not change the version. A breaking change to an existing event's body increments the payload version for that event type only. |
| REST API | `X-GitHub-Api-Version` and the versioned path prefix | Independent of the webhook payload version. A REST version bump never implies a webhook payload bump and the reverse. |

Rules:

- A field added to a payload is announced in the release notes with the event name and the field name, and consumers must ignore unknown fields.
- A field is never removed, renamed, retyped, or made to mean something else inside a payload version. `null` and omission keep the meanings the REST contract defines.
- Identifiers in a payload are stable internal IDs rendered in GitHub's shape. A payload never carries a filesystem path, a database column value that is not part of the public shape, or a secret.
- **Introducing a breaking payload change** requires all of the following, in one release:
  1. A new payload version value for that event type, served when the subscriber's subscription records that version; a subscription that records none keeps receiving the previous version.
  2. Both versions delivered correctly at the same time for at least one minor release, with tests asserting the exact body of each.
  3. A documented cutover date and a deprecation notice, at least two minor releases before the old version stops being served.
  4. A delivery-log entry and a metric for how many subscriptions still use the old version, so removal is a measured decision rather than a guess.
  5. No silent shape change for a subscriber that never opted in. If the old version cannot be kept correct, the change is expressed as a new event name instead of a new payload version.
- Removing a payload version is a release note item and is treated as a breaking API change for the same deprecation window.

## 7. Delivery semantics

Applies to the webhook catalogue only. Audit records are written, not delivered; internal domain events are handled in-process or through the durable outbox.

| Property | Decision |
| --- | --- |
| Guarantee | At-least-once. A delivery may arrive more than once, including after a retry, a redelivery, or a crash between the HTTP response and the recorded outcome. Exactly-once is not promised and is not achievable across a third-party endpoint. |
| Ordering | Not guaranteed. Two deliveries for the same repository may arrive out of order, and a consumer must not depend on `push` arriving before the `pull_request` it caused. |
| Signing | Every delivery is signed with the subscription's shared secret using HMAC-SHA256 over the exact transmitted body, sent in `X-Hub-Signature-256`. HMAC-SHA1 is offered for compatibility and can be disabled per subscription. Comparison is constant-time. The secret is generated by the instance, stored hashed where it is only verified and encrypted where it must be re-presented, shown once at creation and rotation, and never logged, returned in a payload, or included in a delivery record. |
| Enqueue point | The payload is built by the domain module that owns the change and enqueued from the same transaction boundary as the domain change. A committed change has exactly one enqueued delivery per matching subscription, including when the transaction is retried. A transport never builds a payload. |
| Subscriber selection | A subscription's scope (repository, organization, or installation) and event list decide delivery. An event for a private repository is never delivered to a subscription that lacks access to it. |
| Retry | Exponential backoff with jitter for timeouts, connection errors, and `5xx`. No retry for `4xx`, except `429`, which is retried honouring `Retry-After` when present. The attempt count is bounded and configurable; the terminal failure is recorded and visible to an operator. |
| Backoff bound | The schedule is a bounded, documented sequence (for example one immediate attempt, then retries with an increasing delay and jitter), with a maximum attempt count and a maximum total delivery lifetime. A delivery that exhausts either is marked terminally failed. |
| Delivery log | One record per attempt: subscription, event name, delivery identifier, attempt number, request headers (with the signature and secret redacted), request body digest and size, response status, a bounded response-body prefix, error, and duration. The log never stores the subscription secret, a token embedded in a payload field, or the raw payload as a durable record. Records are retained for a documented window and then removed by a retention job. |
| Redelivery | Manual redelivery of one delivery and of a failed batch from the UI and the API. A redelivery recreates a byte-identical body and the same `X-GitHub-Delivery` identifier, so a consumer that already processed it can recognize the duplicate. Redelivery is itself recorded. |
| Consumer obligation | A consumer must be able to deduplicate by the delivery identifier (`X-GitHub-Delivery`). Because delivery is at-least-once and ordering is not guaranteed, a consumer that acts on an event must be idempotent with respect to the identifier it has already seen, and must tolerate an out-of-order or replayed event without corrupting its own state. |
| Verification guidance | A consumer verifies the signature over the raw bytes before parsing, ignores `X-Hub-Signature` when it has an `X-Hub-Signature-256`, rejects a delivery whose signature does not match, and responds `2xx` promptly. A `2xx` that arrives after the retry window is still recorded. |
| Outbound request safety | Delivery targets block loopback, link-local, and cloud metadata addresses unless the operator explicitly allowlists them. The hostname is resolved and the resolved address connected, to prevent DNS rebinding. Redirects are disabled. The response-body read is capped. A timeout applies to connect and to the whole exchange. |
| Delivery failure isolation | A failing delivery never fails the domain change, never blocks another subscription's delivery, and never blocks a request path. |
| Backpressure | A subscription that fails repeatedly is retried under a bounded schedule. Sustained failure is surfaced in the UI and in metrics, and a subscription can be disabled by an administrator, which stops new enqueues without deleting its history. |

## 8. Which catalogue applies when both could

The default rule is: **the audit ledger records the change; webhooks deliver it only where GitHub delivers it.** Both happen, independently, for the events below.

| Change | Audit record | Webhook delivery |
| --- | --- | --- |
| Credential, token, SSH key, or GPG key lifecycle | Yes | No. GitHub does not deliver these, and a delivery would leak credential metadata to a subscriber. |
| OAuth app registration, authorization, or secret rotation | Yes | No |
| GitHub App registration, private key, and permission changes | Yes | No |
| Installation created, suspended, unsuspended, uninstalled | Yes | Yes (`installation`) |
| Installation repository selection changed | Yes | Yes (`installation_repositories`) |
| Repository created, deleted, archived, renamed, visibility changed, transferred | Yes | Yes (`repository`, `create`, `delete`, `public` for the subset GitHub delivers) |
| Collaborator added, role changed, removed | Yes | Only where GitHub delivers a corresponding event (`member` for organization-derived access); a direct-collaborator change is audited and not delivered |
| Branch protection rule or ruleset changed | Yes | Only where GitHub delivers (`repository` `edited`); the rule-level change is audited and not delivered |
| Ruleset bypass used, required check failed a merge | Yes | No |
| Deploy key created or deleted | Yes | Yes (`deploy_key`) |
| Environment created or updated, deployment approved or rejected | Yes | Yes (`deployment_review`, `deployment_status`) |
| Workflow secret or variable created, updated, or deleted | Yes | No. A delivery would disclose that a secret exists and when it changed; the audit record is the evidence. |
| Workflow run and workflow job state changes | Yes | Yes (`workflow_run`, `workflow_job`) |
| Runner registered, removed, or refused for a protocol mismatch | Yes | No |
| OIDC token issued | Yes | No |
| Package published, version deleted, visibility changed | Yes | Yes (`package`, `package_version_deleted`) |
| Release created, edited, deleted, asset changed | Yes | Yes (`release`) |
| Workspace created, started, stopped, rebuilt, deleted, shared, snapshotted, reaped, or refused by quota | Yes | No. Workspaces have no GitHub-compatible webhook surface, and the workspace lifecycle is operator-visible through the audit log and the workspace API. |
| Discussion, project, and issue content changes | Yes | Yes (`discussion`, `discussion_comment`, `projects_v2*`, `issues`, `issue_comment`) |
| Moderation report, disposition, block, and interaction limit | Yes | No |
| Operator actions: migrate, backup, restore, instance settings, legal hold, retention run, node membership | Yes | No |
| Webhook subscription created, updated, deleted, or secret rotated | Yes | No. A subscription change is never delivered to itself or to another subscription. |
| Webhook delivery terminally failed or redelivered | Yes | No |
| E-mail delivery terminally failed or redelivered | Yes | No |

Rules that decide an ambiguous case:

1. **Audit first.** If an action is consequential enough for an operator to need evidence, it produces an audit record whether or not a webhook exists.
2. **Parity governs webhooks.** A webhook event exists where GitHub delivers one, because existing integrations depend on it. Inventing a webhook event with no GitHub counterpart requires a documented reason and lands in this catalogue first.
3. **Secrets are never delivered.** Any event whose payload would reveal a secret's existence, value, rotation instant, or scope is audit-only.
4. **Both surfaces carry the same stable identifier** for the subject, so an operator can join an audit record to a delivery record for the same change without matching on names or timestamps.
5. **Neither surface is authoritative for the other.** A missing delivery does not invalidate the audit record, and an audit record does not imply a delivery was attempted.

## 9. Open questions

- Whether the audit catalogue is exposed as a REST surface under `/api/v1` with the same type strings, or through a separate administrative surface with a fixed type list; the type strings are the contract either way.
- Whether `check_run` and `check_suite` deliveries are produced by the Actions engine directly or by a projection over run state, which decides whether they can be delivered for checks produced through the API without a workflow.
- The exact retention default for webhook delivery records per instance size, and whether the operator can extend it without affecting payload retention, which is not stored.
