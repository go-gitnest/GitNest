# Governance and enterprise administration roadmap
**Phase:** 8
**Status:** not started
**Roadmap file:** docs/roadmap/08-governance-and-enterprise.md

## Purpose

Phases 1-7 make GitNest capable; this phase makes it controllable. It adds the rules that constrain what a merge, a push, or a deployment may do, the identity controls that let an organization trust its accounts (second factors, SSO, provisioning), and the evidence trail an administrator needs afterwards. Every control here is enforced in the shared application services that already make authorization decisions, so a rule cannot be bypassed by using a different transport or a different client. Advanced Security products are explicitly excluded and are not designed in this document.

## Scope

- Repository governance: branch protection rules, repository rulesets with organization inheritance, required status checks integrating with Phase 5, and CODEOWNERS.
- Deployment governance: deploy keys, environments with protection rules and approvals.
- Accountability: organization and enterprise audit log with a queryable API and export.
- Account security: TOTP, WebAuthn security keys, recovery codes, 2FA enforcement policies.
- Enterprise administration: enterprise accounts, policy inheritance, SAML SSO, SCIM provisioning, and LDAP directory integration.
- Community moderation: reporting, blocking, interaction limits, per-account-type rate limits.
- Legal and retention controls: data export, retention windows, legal hold, and deletion.

## Workstreams

### W8.1 Branch protection rules
- **Deliverable:** Per-branch (and per-pattern) protection settings covering: block force pushes, block deletions, require a pull request before merging, required approving review count, dismiss stale approvals on new commits, require review from CODEOWNERS, require conversation resolution, require linear history, require signed commits, and restrict who may push. Rules are evaluated in the pull-request merge service and in the Git receive path, not in the web UI.
- **Depends on:** Stage 3.3 merge service; stage 1.3 permission service; `internal/gittransport`.
- **Acceptance:** Tests prove a direct push to a protected branch is rejected by the Git transport for an authorized writer while the same user can merge through a pull request; force push and delete are rejected; the required review count is enforced against distinct reviewers, and an approval by the pull request author does not count toward it.

### W8.2 Repository rulesets and organization inheritance
- **Deliverable:** Rulesets as ordered, named rule collections with a target (branch patterns, tag patterns, or all refs), an enforcement status (`active`, `evaluate`, `disabled`), a bypass list (roles, teams, or apps), and an optional scope limited to selected repositories. Organization rulesets apply to member repositories and can be required by policy so a repository owner cannot disable them. Precedence is documented: the most restrictive applicable rule wins.
- **Depends on:** W8.1; `internal/organization`.
- **Acceptance:** Tests cover overlapping rulesets resolving to the most restrictive outcome, an `evaluate` ruleset recording a would-have-blocked event without blocking, the same ruleset applied to a branch and a tag, a repository owner being unable to weaken an organization-required ruleset through the API or UI, and bypass entries being audited when used.

### W8.3 Required status checks
- **Deliverable:** Required check names and optional required app bindings on protection rules and rulesets, evaluated against the check-run and commit-status data produced by Phase 5, with a documented rule for which check result is authoritative for a given commit SHA. Failed, cancelled, skipped, neutral, and missing checks each have a defined effect, and `strict` mode requires the branch to be current with the base before merging.
- **Depends on:** Phase 5 Actions status data; W8.1, W8.2.
- **Acceptance:** Tests cover merge blocked on a failing required check, on a missing required check, on a cancelled check, and allowed on a passing one; `strict` mode blocks a merge whose base advanced and permits it after the branch is updated; a check with a duplicate name from a different app does not satisfy a required app-bound check.

### W8.4 CODEOWNERS
- **Deliverable:** A parser for `.github/CODEOWNERS`, `CODEOWNERS`, and `docs/CODEOWNERS` (first match wins in that order) supporting pattern syntax GitHub clients expect, comments, multiple owners per line, and team owners. Resolution to a user or team for a changed path drives review requests and, where W8.1 requires it, the approving-review requirement. Invalid files produce a visible error in the repository settings rather than a silent no-op.
- **Depends on:** W8.1; `internal/pullrequest`; `internal/organization`.
- **Acceptance:** Tests cover pattern precedence, negative patterns, escaping, a malformed line surfacing an error, a team owner expanding to current team members at evaluation time rather than at request time, and a CODEOWNERS-required approval being satisfied only by a listed owner.

### W8.5 Deploy keys
- **Deliverable:** Repository-scoped SSH keys with read-only or read-write access, one repository per key, fingerprint display, last-used time, and revocation, exposed in the API defined in Phase 4 and enforced through the W4.11 permission decision. Keys are stored so the private key never reaches GitNest; only the public key and fingerprint are persisted.
- **Depends on:** W4.11, W4.14; `internal/transport/ssh`.
- **Acceptance:** Tests prove a deploy key cannot read or write a second repository even when its owner can, a read-only key cannot push, a revoked key fails on the next connection, and key use appears in the audit log without key material.

### W8.6 Environments and deployment protection rules
- **Deliverable:** Named environments per repository with protection: required reviewers (up to a documented maximum) with a wait timer, branch and tag restrictions on which refs may deploy, required status checks for the deploying ref, and a secrets-and-variables scope bound to the environment. A deployment is a recorded attempt with a state (`pending`, `approved`, `rejected`, `in_progress`, `success`, `failure`, `cancelled`), a review decision trail, and a deployment history in the UI and API.
- **Depends on:** Phase 5 runner and workflow jobs; W4.11.
- **Acceptance:** Tests cover a deployment blocked pending review until an authorized reviewer approves, a rejection preventing the job from starting, a deployment from a disallowed branch being refused before any runner is scheduled, and a self-approval being rejected when the requesting user is also the required reviewer and no other reviewer is available.

### W8.7 Audit log
- **Deliverable:** Append-only audit records at organization and enterprise scope covering credential and token lifecycle, 2FA and SSO changes, membership and role changes, repository visibility and transfer, ruleset and protection changes, environment approvals, deploy key and app installation changes, package and release deletions, policy changes, and administrative reads of sensitive settings. Each record carries actor, principal kind, action, target, timestamp, source address and user agent where applicable, and a structured detail blob. A query API supports filters by actor, action, target, date range, and an export job produces a signed, tamper-evident file. Records are never editable through any API and are removed only by retention or legal-hold policy.
- **Depends on:** `internal/activity`; W4.11; organization and enterprise models.
- **Acceptance:** Tests prove no API path can update or delete an audit record, that every mutation in the covered action list emits exactly one record, that export verifies against its checksum, that a filtered query returns the same rows as the equivalent sequential scan of the ledger, and that a retention job refuses to delete records under legal hold.

### W8.8 TOTP two-factor authentication
- **Deliverable:** TOTP enrollment with a QR code, a confirmation step that requires a valid code before the factor activates, a drift-tolerant verification window, per-user recovery via an administrator reset, and rate-limited verification attempts. Sessions started before a factor was added are downgraded so the factor is required at the next sensitive action. Secrets are encrypted at rest with the instance key and never logged or returned by any API.
- **Depends on:** `internal/identity`; W4.8.
- **Acceptance:** Tests cover enrollment requiring a valid code to activate, replay of a used code within the same window being rejected, a wrong code not activating the factor, throttling after repeated failures, and a login for a factor-enabled account not issuing a full session before the factor is verified.

### W8.9 WebAuthn security keys
- **Deliverable:** WebAuthn registration and assertion for security keys and platform authenticators, multiple keys per account, named credentials, sign-counter handling, and revocation. The relying-party ID is derived from the configured public URL; attestation is not required and is stored only if the operator opts in. Passwordless sign-in is not enabled in this workstream.
- **Depends on:** W8.8 (shared second-factor session state).
- **Acceptance:** Tests using a virtual authenticator cover registration, assertion, a cloned-key counter regression being rejected, revocation taking effect on the next sign-in, and a challenge being single-use and bound to the session.

### W8.10 Recovery codes and account recovery
- **Deliverable:** Single-use recovery codes generated on request, shown once, stored hashed, regenerated as a set that invalidates the previous set, and consumed on use with an audit entry. A documented administrator path for an account that lost every factor, requiring a recent, audited administrative action on the server and a forced re-enrollment of all factors afterwards.
- **Depends on:** W8.8, W8.9.
- **Acceptance:** Tests prove a code works once and fails on reuse, regenerating codes invalidates all previous codes, and the administrator recovery path forces re-enrollment and is recorded in the audit log.

### W8.11 Two-factor enforcement policies
- **Deliverable:** Enforcement at organization and enterprise scope: un-enrolled members either lose organization access or can only access through a grace period the operator sets, with a visible list of non-compliant members. A personal account outside any enforcing organization is unaffected. Enforcement interacts with SSO login and with machine users, which are exempt by kind rather than by name.
- **Depends on:** W8.8, W8.9, W8.12.
- **Acceptance:** Tests cover access being denied exactly when the grace period ends, the exemption for machine users, a compliant member being unaffected, and enforcement changes producing audit records and notifications to affected members.

### W8.12 SAML SSO
- **Deliverable:** SAML 2.0 service-provider support per organization: identity provider metadata upload, SP metadata generation, signed assertion validation with a pinned certificate, `NameID` and attribute mapping to username and email, an email-domain association, and just-in-time provisioning of a member with the least privilege the mapping allows. Authenticated sessions carry the SSO authorization so a subsequent IdP-side revocation can be enforced; a documented maximum session lifetime for SSO sessions is enforced. An organization can require SSO for all members.
- **Depends on:** `internal/identity`; `internal/organization`.
- **Acceptance:** Tests with a stub IdP cover successful assertion, an unsigned or wrongly signed assertion being rejected, an expired assertion, an assertion with a replayed ID, an email outside the associated domain, JIT provisioning with the mapped role, and a member whose SSO session is revoked losing organization-scoped access.

### W8.13 SCIM provisioning
- **Deliverable:** SCIM 2.0 endpoints for users and groups authorized by a per-organization bearer token: create, read, update, patch, deactivate, and list with SCIM filtering and pagination. Group membership maps to teams, deactivation revokes sessions and repository access without deleting authored content, and an unmapped external group is reported rather than silently ignored. Provisioning never unlocks a role above the organization owner's own grant.
- **Depends on:** W8.12; `internal/organization`.
- **Acceptance:** Tests cover create-then-login, deactivate immediately revoking access and sessions while preserving commit authorship, group-to-team mapping and unmapping, idempotent repeat delivery of the same SCIM operation, a token scoped to one organization being unable to affect another, and a role escalation attempt being clamped.

### W8.14 Enterprise accounts and policy inheritance
- **Deliverable:** An enterprise container above organizations with member organizations, owners, billing-independent settings, and policies that organizations may be required to follow: 2FA enforcement, SSO requirement, allowed repository visibilities, ruleset requirements, allowed app installations, credential policies (token expiry and IP allow lists), and an organization creation policy. Inheritance is explicit: each organization shows which settings are inherited, which are set locally, and which local settings conflict with an enterprise requirement.
- **Depends on:** W8.11, W8.12, W8.2.
- **Acceptance:** Tests prove a conflicting local setting cannot override an enterprise requirement, that changing an enterprise policy propagates to member organizations on the next request without a deployment, that leaving an enterprise restores local control without data loss, and that each inherited setting is reported as inherited through the settings API.

### W8.15 Moderation controls
- **Deliverable:** Abuse reporting on repositories, issues, pull requests, comments, and user profiles with a queue and dispositions; user blocking, which removes the blocked user's content from the blocker's view and prevents new interactions; temporary and permanent interaction limits per repository or organization; and per-account-type rate limits distinguishing interactive users, machine users, and installation tokens. All moderation actions are audited and reversible where the underlying content is not deleted.
- **Depends on:** W8.7; `internal/activity`; W4.4.
- **Acceptance:** Tests prove a blocked user cannot comment on or subscribe to the blocker's content while still being able to read public content, an interaction limit blocks new interactions from accounts younger than the threshold without deleting existing content, a report disposition is audited, and a moderation action is idempotent.

### W8.16 Legal, retention, and data controls
- **Deliverable:** Documented and enforced retention windows for audit records, webhook deliveries, job history, and notification data; organization and enterprise data export producing a documented archive of metadata and repository references; legal hold that suspends deletion for a defined scope; account deletion that anonymizes authored content where policy requires it while preserving repository integrity; and a "delete repository" flow that removes metadata, storage, packages, and release assets as one audited operation with an explicit, reported outcome.
- **Depends on:** W8.7; `internal/repository`; W6.12.
- **Acceptance:** Tests prove a retention job deletes only records outside the window, legal hold blocks both retention and account deletion for the held scope, an export archive contains the documented set of records and verifies against its manifest, and a repository deletion leaves no reachable repository, package blob, or release asset.

### W8.17 LDAP directory connection and authentication
- **Status:** not started
- **Deliverable:** Directory configuration in `internal/identity`: one or more server URIs with failover, LDAPS or StartTLS with certificate validation, a bind account whose password is encrypted at rest, a user search base and filter with attribute mapping for username, email, and display name, a connection test that reports the exact failure without echoing the credential, and password authentication by binding as the user so no directory password is ever stored.
- **Depends on:** W8.7; the phase 1 account model; [../architecture/secrets.md](../architecture/secrets.md)
- **Acceptance:** A directory sign-in creates or links the account and persists no directory password; a wrong password and an unknown user fail with the same generic error as a local sign-in; certificate-validation failure, unreachable server, and bad bind credentials each produce a distinguishable operator-facing error containing no credential; every connection test and configuration change is audited.

### W8.18 LDAP group mapping, provisioning, and identity-path precedence
- **Status:** not started
- **Deliverable:** Group search and group-to-team mapping with a bounded periodic sync job plus a manual resync action; just-in-time account creation on first directory sign-in; deactivation and session revocation when a user disappears from the directory; reconciliation that never deletes a locally created team or a locally granted membership it does not own; and documented precedence when one account could authenticate through a local password, the directory, SAML, or SCIM.
- **Depends on:** W8.17; W8.13; `internal/organization` teams
- **Acceptance:** Tests prove a mapping grants and revokes team access on sync, a directory removal deactivates the account and revokes its sessions, a sync failure applies nothing and is audited, a locally created team survives a sync, and the documented precedence holds for an account present in both the directory and a local password store.

## Data and interfaces

Schema additions, all forward-only migrations in `migrations/`:

- `branch_protection_rules`, `rulesets`, `ruleset_rules`, `ruleset_bypass_entries`, `ruleset_repositories`.
- `required_status_checks`, `codeowners_errors`, `check_requirements` (app binding per required check).
- `deploy_keys` (repository id, public key, fingerprint, read-only, last used, created by) — shared with W4.14.
- `environments`, `environment_protection_rules`, `environment_reviewers`, `environment_secrets_scope`, `deployments`, `deployment_reviews`.
- `audit_log` (append-only; actor, principal kind, action, target type, target id, organization id, enterprise id, source address, user agent, detail JSONB, created at) with an index strategy for the documented query filters.
- `webauthn_credentials`, `totp_secrets`, `recovery_codes`, `two_factor_enforcement` (scope type, scope id, grace period, state).
- `saml_configurations`, `saml_sessions`, `scim_tokens`, `scim_group_mappings`.
- `ldap_configurations` (server URIs, TLS mode and pinned certificate, bind DN, encrypted bind password, search bases and filters, attribute mapping, sync interval), `ldap_group_mappings`, `ldap_sync_state`.
- `enterprises`, `enterprise_members`, `enterprise_organizations`, `enterprise_policies`, `organization_policy_overrides`.
- `abuse_reports`, `report_dispositions`, `user_blocks`, `interaction_limits`, `account_rate_limit_tiers`.
- `retention_policies`, `legal_holds`, `data_export_jobs`, `deletion_requests`.

Packages touched: `internal/identity` (2FA, WebAuthn, recovery, SAML, SCIM, LDAP), `internal/organization` (policies, SSO, provisioning, moderation scope), `internal/repository` (protection rules, rulesets, environment references, deploy keys), `internal/pullrequest` (protection and CODEOWNERS evaluation at merge), `internal/gittransport` and `internal/transport/{http/git,ssh}` (ref-level rule enforcement on push), `internal/actions` and `internal/runner` (status data, deployment gating), `internal/activity` (audit records and retention), `internal/jobs` (retention, export, deletion, reconciliation), `internal/transport/http/{web,api}` (settings, audit views, moderation), `internal/packages` (deletion participation), `internal/webhooks` (governance events and delivery retention).

API surface: ruleset and protection endpoints, CODEOWNERS errors, environment and deployment endpoints, audit log query and export, 2FA enrollment and verification, SSO and SCIM endpoints, LDAP directory endpoints (configuration, connection test, manual resync), enterprise policy endpoints, moderation endpoints, and retention/legal-hold/deletion endpoints — all registered in the Phase 4 permission registry so Phase 4 credentials and apps can call them under the same decision function.

Events: `branch_protection_rule`, `repository_ruleset`, `deploy_key`, `environment`, `deployment_status`, `member`, `team`, `org_block`, `two_factor_enforcement`, `saml_sso`, `scim_provision`, `ldap_directory`, `ldap_sync`, `audit_log_export`, `data_retention`.

## Security notes

- Governance enforcement lives in the services that mutate state — the merge service, the Git receive path, the deployment gate — and never only in a settings screen or a React check.
- The most restrictive applicable rule wins; a bypass is an explicit, audited grant, never an implicit role property.
- Second-factor material is encrypted at rest with the instance key and excluded from logs, API responses, exports, and audit detail fields.
- WebAuthn relying-party ID and origin are derived from the configured public URL, never from request headers, so a proxy or a hostile Host header cannot widen the origin.
- SAML assertions are validated for signature, audience, recipient, issue/expiry instants, and replay; IdP certificates are pinned and rotation is an explicit operator action.
- SCIM tokens are per-organization, hashed, scopable to a narrow action set, and revocable; a SCIM write can never grant a role beyond the mapping's ceiling.
- Directory binds use TLS with certificate validation. The bind password is encrypted with the instance key and never returned, logged, or included in an export, and a user's directory password is used for one bind attempt and never stored.
- Interaction limits and rate-limit tiers are limits on new actions, not retroactive deletions; moderation actions are reversible unless the content was removed under policy.
- Enterprise policies are evaluated server-side on every request; a UI that hides an action is a convenience, not the control.
- Audit records and exports are append-only; retention and legal hold are enforced in the same code path so a manual SQL delete is the only way to lose a record, and that requires database access.
- Deletion flows report a per-subsystem outcome so a partially completed deletion is visible and repairable rather than silently assumed complete.
- GitNest must not become a general-purpose data processor for tenant content beyond what retention and export explicitly cover; exports are scoped and audited.

## Out of scope

- **Advanced Security products are out of scope for this phase and must not be designed here:** code scanning, CodeQL, secret scanning and push protection, Dependabot alerts, Dependabot security updates, dependency review, and any security-advisory or vulnerability-database feature.
- Automated code review, branch-age or commit-message linters, and policy enforcement that inspects code content.
- Billing, metering, and seat-based licensing.
- GitHub Enterprise Server management consoles, HA clustering, and multi-node governance propagation beyond the single-node baseline.
- Any content-hosting or takedown workflow beyond the moderation queue defined in W8.15.
- Impersonation of users by administrators, and any break-glass path that skips audit recording.

## Exit criteria

- Protected branches and rulesets are enforced on both the Git push path and the merge path, with denied-case tests for each rule they define.
- Required status checks from Phase 5 gate merges as documented, including the missing-check and stale-base cases.
- A deployment to a protected environment cannot start without its required approvals, and the approval trail is visible in the UI and API.
- Every action in the W8.7 covered list produces exactly one immutable audit record, and the export verifies against its checksum.
- TOTP, WebAuthn, and recovery codes all work, enforcement policies behave as documented, and no factor secret is exposed through a log, an API response, or an export.
- SAML SSO and SCIM provisioning work against a stub IdP, with JIT provisioning and deactivation covered by tests.
- LDAP sign-in, group-to-team sync, directory deactivation, and identity-path precedence work against a stub directory, with TLS-validation and unreachable-server cases covered and no directory credential stored.
- Enterprise policies override conflicting organization settings, and each inherited setting is reported as inherited.
- Retention, legal hold, export, and deletion behave as documented, with tests proving a repository deletion removes its packages and release assets as well as its metadata and Git storage.
