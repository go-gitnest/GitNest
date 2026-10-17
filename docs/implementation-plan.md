# Implementation plan

This plan turns the product goals into ordered phases with reviewable workstreams. It is written to be worked continuously, one workstream at a time: read this file, read the roadmap file for the active phase, pick the next unblocked workstream, implement it, verify it, update the status, repeat. Each round leaves the tree in a state a reviewer can read.

Every phase must leave the main branch buildable, and every change that alters commands, configuration, or upgrade behaviour must update the README in the same change.

## Related documents

- [product-goals.md](product-goals.md) — positioning, parity definition, decided exclusions, definition of done.
- [architecture.md](architecture.md) — module boundaries, storage, security model, runtime topology.
- `roadmap/*.md` — per-phase workstreams, acceptance criteria, and out-of-scope notes.
- Cross-cutting design that every phase must honour:
  - [architecture/configuration.md](architecture/configuration.md) — where each setting lives and how it changes
  - [architecture/secrets.md](architecture/secrets.md) — password hashing, token digests, encrypted secrets, key management
  - [architecture/data-model.md](architecture/data-model.md) — naming, typing, indexing, concurrency, migration rules
  - [architecture/frontend.md](architecture/frontend.md) — server rendering, accessibility, internationalization, browser support
  - [architecture/urls-and-identifiers.md](architecture/urls-and-identifiers.md) — route and identifier contract
  - [architecture/testing.md](architecture/testing.md) — test tiers, gates, and obligations
  - [architecture/performance.md](architecture/performance.md) — budgets and limits
  - [architecture/administration.md](architecture/administration.md) — operator console and CLI surfaces
  - [architecture/packaging.md](architecture/packaging.md) — build, release, upgrade, and how assets ship
  - [reference/events.md](reference/events.md) — audit and webhook event catalogues
  - [security/threat-model.md](security/threat-model.md) — per-surface threats and required mitigations
  - [glossary.md](glossary.md) — shared vocabulary
  - [parity-checklist.md](parity-checklist.md) — the auditable inventory behind the 1:1 promise

## Current status

**Phase 0 is in progress.** The module skeleton and the command entry point exist; every other workstream is not started, and no product capability exists yet. The implementation is built from these documents in the order below.

The plan is written so that the first phase produces a running, configurable, testable service with no product features, and each later phase adds one security boundary or one product area.

| Phase | Status | Notes |
| --- | --- | --- |
| 0 Service foundation | **All workstreams implemented** | W0.1–W0.10 implemented. Exit criteria verified locally; the hosted CI run is the one item still unconfirmed |
| 1 Identity and repository hosting | **Implemented** | W1.1–W1.8, W1.9a/b/c-1/c-2, W1.10a/b implemented (configuration, administrator bootstrap, accounts/sessions/invitations, the browser and API session surface, repository metadata and the shared permission service, bare repository storage and the bounded Git runner, clone and fetch, push with write-permission enforcement, the repository read API, the server-rendered read pages, the audit ledger and repository settings, the settings HTTP surface, the frontend build pipeline, the interactive shell with repository creation, and the settings surface with a CSRF token issued per document load). W1.9 and W1.10 were each split because their deliverables named more than one round can carry. The LFS object store and its batch API, the single-node backup and restore procedure, and the deployment skeleton with its operator documentation are implemented. **Every workstream W1.1 through W1.13 is implemented, and the read surfaces are now styled with the same token set as the interactive ones.** The phase stays "in progress" because its exit criteria depend on the gaps below, most sharply the missing personal access token. **Four gaps are open and need a maintainer decision:** no workstream delivers a personal access token, so a deployed instance cannot authenticate a pusher; the credential model is phase 4; a repository name or description change has no auditable type; and `GITNEST_SECRET_KEY` is not implemented, with the case for deferring it to its first consumer recorded in the roadmap | Accounts, sessions, invitations, repository metadata and permissions, storage, Git over HTTPS, browser, LFS, backup |
| 2 Collaboration | **In progress** | W2.1, W2.2a–c, W2.3, W2.4a/b, and W2.5a implemented (the Markdown renderer with raw HTML disabled; the issue domain with gap-free per-repository numbering and the lifecycle rules; the issue API, read pages, and mutation forms; issue comments with an append-only edit history; labels with case-folded uniqueness and a grammar-validated colour; milestones with their own never-reused sequence, derived progress, and a calendar-date due date; issue templates read strictly from `.github/ISSUE_TEMPLATE` with a picker and a contact-link configuration). **W2.5b (assignees) is blocked on a product decision.** The rest of the phase remains |
| 3 Pull requests | Not started | Compare, review, merge, merge queue |
| 4 API and extensibility | Not started | REST, GraphQL, tokens, OAuth apps, GitHub Apps, webhooks |
| 5 Actions | Not started | Workflows, runners, secrets, artifacts, caches |
| 6 Packages and releases | Not started | Releases, OCI and language registries |
| 7 Discovery and surfaces | Not started | Search, topics, stars, wiki, Pages, discussions, projects, insights |
| 8 Governance and enterprise | Not started | Branch protection, rulesets, 2FA, SSO/SAML, SCIM, audit, moderation |
| 9 Cloud development | Not started | Workspaces: lifecycle, IDE and terminal, port forwarding, quotas |
| 10 Operations and scale | Not started | Multi-node, object storage, search service, observability, upgrades |

## Cross-cutting requirements

These apply to every phase and are enforced in review, not deferred to the phase that happens to depend on them. Each links to the document that owns the detail.

| Requirement | Owner document |
| --- | --- |
| A setting lives in exactly one configuration layer; policy is database-backed and validated | [architecture/configuration.md](architecture/configuration.md) |
| A value that is only compared is hashed; a value that is replayed is encrypted; keys are never in a backup with their ciphertext | [architecture/secrets.md](architecture/secrets.md) |
| Conventions for identifiers, naming, typing, indexing, concurrency, pagination, retention, and migration compatibility | [architecture/data-model.md](architecture/data-model.md) |
| Read surfaces are server-rendered; interactive surfaces are React; accessibility and i18n rules apply from the first frontend workstream | [architecture/frontend.md](architecture/frontend.md) |
| Routes and identifiers follow one contract, including redirects on rename | [architecture/urls-and-identifiers.md](architecture/urls-and-identifiers.md) |
| Tests follow the tier and gate rules, including container, client-conformance, and isolation tests | [architecture/testing.md](architecture/testing.md) |
| Budgets and limits are set before the surface that needs them | [architecture/performance.md](architecture/performance.md) |
| Operator-facing capability ships with a console or CLI surface and an audit event | [architecture/administration.md](architecture/administration.md) |
| Every phase leaves a buildable, releasable instance with a documented upgrade path | [architecture/packaging.md](architecture/packaging.md) |
| Consequential changes emit the catalogued audit events; deliverable events follow the webhook catalogue and payload versioning | [reference/events.md](reference/events.md) |
| Each untrusted surface ships with its threat model, mitigations, and both allowed and denied tests | [security/threat-model.md](security/threat-model.md) |
| Terminology stays consistent with the glossary; the parity checklist is updated when a capability lands or is deliberately excluded | [glossary.md](glossary.md), [parity-checklist.md](parity-checklist.md) |

## Phase index

Estimates are continuous focused work by one implementer including tests and documentation, and assume no major rework. They are for planning, not commitments.

| Phase | Theme | Roadmap | Estimate |
| --- | --- | --- | --- |
| 0 | Service foundation | [00](roadmap/00-service-foundation.md) | 10–15 h |
| 1 | Identity, accounts, repository hosting, Git over HTTPS, backup | [01](roadmap/01-identity-and-repository-hosting.md) | 50–75 h |
| 2 | Issues, organizations, teams, notifications, SSH | [02](roadmap/02-collaboration.md) | 45–65 h |
| 3 | Pull requests, review, merge | [03](roadmap/03-pull-requests.md) | 30–45 h |
| 4 | REST, GraphQL, tokens, OAuth and GitHub Apps, webhooks | [04](roadmap/04-api-and-extensibility.md) | 60–90 h |
| 5 | Actions | [05](roadmap/05-actions.md) | 120–200 h |
| 6 | Packages and releases | [06](roadmap/06-packages-and-releases.md) | 70–120 h |
| 7 | Discovery and content surfaces | [07](roadmap/07-discovery-and-surfaces.md) | 80–130 h |
| 8 | Governance and enterprise | [08](roadmap/08-governance-and-enterprise.md) | 120–200 h |
| 9 | Cloud development | [09](roadmap/09-cloud-development.md) | 250–450 h |
| 10 | Operations and scale | [10](roadmap/10-operations-and-scale.md) | 70–120 h |

Total for the full parity surface: roughly 905–1510 hours of focused work, before rework. The largest single item by far is phase 9: a Codespaces-class subsystem is an orchestration product in its own right and needs its own isolation, scheduling, storage, and lifecycle design.

## Phase 0: service foundation

**Purpose:** Make the process predictable to configure, start, inspect, and stop.

**Detail:** [roadmap/00-service-foundation.md](roadmap/00-service-foundation.md)

| Workstream | Status |
| --- | --- |
| W0.1 Module scaffolding and command entry point | Implemented |
| W0.2 Configuration loading and validation | Implemented |
| W0.3 PostgreSQL connectivity | Implemented |
| W0.4 Migration runner | Implemented |
| W0.5 Health and readiness endpoints | Implemented |
| W0.6 Structured logging | Implemented |
| W0.7 Bounded shutdown | Implemented |
| W0.8 Local development database recipe | Implemented |
| W0.9 Integration test harness | Implemented |
| W0.10 CI pipeline | Implemented |

**Exit criteria:** A contributor can follow the README on a clean machine, start PostgreSQL without installing it globally, apply migrations twice with the second run a no-op, force a failing migration and see both its schema work and ledger row roll back, start the service, observe `/api/health` and `/api/ready`, stop it with an interrupt, and see it exit within the configured deadline. CI runs formatting, vet, build, and race tests against a real database. No product feature exists yet, and no placeholder package has been created for one.

**Verified locally (each with the command output in the round that did it):**

| Criterion | Evidence |
| --- | --- |
| Migrations apply, a second run is a no-op, a failure rolls back schema and ledger | `gitnest migrate` against a fresh database reported `applied 1, skipped 0` then `applied 0, skipped 1`; the `postgres` suite covers the rollback case, the edited-migration case, and two concurrent runners |
| Probes behave | `/api/health` returned `200` while PostgreSQL was stopped, `/api/ready` returned `503` with a body of `{"status":"unavailable","version","commit"}` and no dependency detail, then returned to `200` when the database came back |
| A delivered interrupt exits within the deadline and closes the listener | a console interrupt produced exit status `0` in `0.01s` against a `5s` deadline, and the port stopped answering; an in-flight request is drained, and one that outlives the deadline forces the shutdown and reports it |
| Formatting, vet, build, and race tests pass | all four gates pass on the tree; each was also verified to fail on an injected defect |
| No product feature, no placeholder package | the tree holds `cmd/gitnest` and the phase 0 support packages only |

**Remaining before this phase is closed:**

1. Confirm the first hosted CI run. The workflow exists and its gates were verified locally, but it has never run on a runner.
2. Start the development database with `docker compose up -d postgres` once on a machine with a Docker daemon. The recipe validates and its credentials were exercised against a real server, but the container itself has not been started here.

## Phase 1: identity and repository hosting

**Purpose:** Make GitNest useful for a developer with one account and one repository.

**Detail:** [roadmap/01-identity-and-repository-hosting.md](roadmap/01-identity-and-repository-hosting.md)

| Workstream | Status |
| --- | --- |
| W1.1 Configuration and public URL | Implemented (by W0.2 and W0.6) |
| W1.2 Administrator bootstrap | Implemented |
| W1.3 Accounts, invitations, sessions (domain and persistence) | Implemented |
| W1.4 Browser and API session surface (cookies, CSRF, throttling) | Implemented |
| W1.5 Repository metadata and permission service | Implemented |
| W1.6 Bare repository storage and Git command runner | Implemented |
| W1.7 Git over HTTPS: clone and fetch | Implemented |
| W1.8 Git over HTTPS: push | Implemented |
| W1.9 Repository browser and API | Split into W1.9a/b/c |
| W1.9a Repository read API | Implemented |
| W1.9b Repository read pages | Implemented |
| W1.9c Interactive repository surfaces | Split into W1.9c-1/c-2 |
| W1.9c-1 Build pipeline and the interactive shell | Implemented |
| W1.9c-2 CSRF over a page load and the settings surface | Implemented |
| W1.10 Collaborator management and repository settings | Split into W1.10a/b |
| W1.10a Audit ledger and repository settings | Implemented |
| W1.10b Repository settings HTTP surface | Implemented |
| W1.11 Git LFS | Split into W1.11a/b |
| W1.11a Content-addressed LFS object store | Implemented |
| W1.11b LFS batch API and HTTP surface | Implemented |
| W1.12 Backup and recovery | Implemented |
| W1.13 Deployment skeleton and operator documentation | Implemented |
| W1.14 Personal access tokens | Implemented |

**Exit criteria:** A fresh instance can be bootstrapped by an operator. An invited contributor can sign in, create a private repository, push a local branch, browse the result, and clone it from a second working copy. The owner can grant and revoke collaborator access. Anonymous users can read public repositories but cannot read private ones. Tests exercise denied access through every implemented transport. A tested backup restores the instance.

Do not start organizations, issues, or pull requests until these checks pass. Git transport and repository authorization are the security boundary of the product.

## Phase 2: collaboration

**Purpose:** Let people coordinate changes against hosted repositories, and let organizations own them.

**Detail:** [roadmap/02-collaboration.md](roadmap/02-collaboration.md)

**Exit criteria:** A team can grant repository access through organization membership and teams, file and discuss an issue with labels and milestones, clone and push through both HTTPS and SSH, receive an in-app notification for activity relevant to them, and see who made each change in an audit trail.

## Phase 3: pull requests

**Purpose:** Support a reviewed change from branch comparison to merge.

**Detail:** [roadmap/03-pull-requests.md](roadmap/03-pull-requests.md)

**Exit criteria:** A reviewer can inspect a change, leave line comments, submit a review, and merge an allowed pull request. Stale refs, changed heads, and denied permissions return clear errors without changing repository refs. Merge-queue and auto-merge behaviour is defined and tested.

## Phase 4: API and extensibility

**Purpose:** Make the platform drivable and extensible by external software, with GitHub-compatible contracts.

**Detail:** [roadmap/04-api-and-extensibility.md](roadmap/04-api-and-extensibility.md)

**Exit criteria:** A GitHub API client can authenticate with a personal access token, paginate, use conditional requests, and receive rate-limit headers and stable error bodies. Webhooks are signed, delivered durably, retried, and inspectable. OAuth apps and GitHub Apps can be installed and act with scoped installation tokens. GraphQL serves the same authorization decisions as REST.

## Phase 5: Actions

**Purpose:** Run CI/CD for user repositories without ever trusting user code with platform credentials.

**Detail:** [roadmap/05-actions.md](roadmap/05-actions.md)

**Exit criteria:** A repository with a checked-in workflow runs on push and on pull request, reports job and step status back to the pull request, uploads and downloads artifacts, restores a cache, reads secrets without leaking them into logs, runs a matrix, honours concurrency groups, and can be cancelled and rerun. Untrusted jobs run outside the web process, with a scoped token, resource limits, and deterministic cleanup.

## Phase 6: packages and releases

**Purpose:** Host release artifacts and package registries.

**Detail:** [roadmap/06-packages-and-releases.md](roadmap/06-packages-and-releases.md)

**Exit criteria:** A repository can publish and consume a container image and at least an npm package with authenticated access control, releases carry assets with retention limits, and package permissions follow repository permissions.

## Phase 7: discovery and content surfaces

**Purpose:** Make content findable and host the surfaces teams expect: search, wiki, pages, discussions, projects, insights.

**Detail:** [roadmap/07-discovery-and-surfaces.md](roadmap/07-discovery-and-surfaces.md)

**Exit criteria:** Search returns results for repositories, code, issues, and users with qualifiers, and never returns private content to an unauthorized caller. A repository wiki is Git-backed and versioned. Pages serves a built site over HTTPS. Discussions and Projects are usable with permissions consistent with issues.

## Phase 8: governance and enterprise

**Purpose:** Let an organization enforce policy and meet administrative obligations.

**Detail:** [roadmap/08-governance-and-enterprise.md](roadmap/08-governance-and-enterprise.md)

**Exit criteria:** Branch protection and rulesets block disallowed pushes, required status checks gate merges, CODEOWNERS requests the right reviewers, two-factor authentication can be enforced organization-wide, SAML SSO and SCIM provisioning work against a test identity provider, and the audit log records every consequential change. Advanced Security products remain out of scope.

## Phase 9: cloud development

**Purpose:** Provide hosted development environments, replacing both Codespaces and Gist-style scratch spaces.

**Detail:** [roadmap/09-cloud-development.md](roadmap/09-cloud-development.md)

**Exit criteria:** A user can start a workspace from a repository or a blank scratch space, get a browser IDE and terminal authenticated by their GitNest identity, reach a forwarded port over an access-controlled preview URL, stop and resume with a persistent volume, and have the workspace reaped when idle. Workspaces cannot reach GitNest's database or another tenant's data, and quotas are enforced.

## Phase 10: operations and scale

**Purpose:** Make installation, upgrade, troubleshooting, and recovery practical beyond a single small node.

**Detail:** [roadmap/10-operations-and-scale.md](roadmap/10-operations-and-scale.md)

**Exit criteria:** A release can be installed and upgraded from documented steps with migration locking. Multiple web nodes can run against shared storage with the documented single-writer rules. A maintainer can restore a tested backup, identify a failed Git operation or job without seeing private content, rotate credentials without editing the database by hand, and read metrics for database, Git, job, runner, and workspace health.

## Dependency order

```text
Phase 0: configuration, PostgreSQL, migrations, health, logging, shutdown, CI    [start here]
  -> W1.1 configuration and public URL
  -> W1.2 administrator bootstrap
  -> W1.3 account, session, and invitation model
  -> W1.4 browser/API session surface (cookies, CSRF, throttling)
  -> W1.5 repository metadata and shared permission service
  -> W1.6 bare repository storage and bounded Git command runner
  -> W1.7 Git over HTTPS: clone and fetch
  -> W1.8 Git over HTTPS: push
  -> W1.9 repository browser and API            (brings the frontend, forcing the rendering and i18n decisions)
  -> W1.10 collaborators and repository settings
  -> W1.11 Git LFS
  -> W1.12 backup and restore drill
  -> W1.13 deployment skeleton and operator documentation
  -> phase 2 issues, organizations, teams, notifications, SSH
  -> phase 3 pull requests, review, merge
  -> phase 4 REST, GraphQL, tokens, OAuth apps, GitHub Apps, webhooks
  -> phase 5 Actions            (needs 1.6 storage, 3 checks reporting, 4 tokens)
  -> phase 6 packages, releases (needs 4 registry auth)
  -> phase 7 search, wiki, pages, discussions, projects
  -> phase 8 governance, SSO, audit
  -> phase 9 cloud development  (needs 1.6 storage, 5 isolation primitives, 8 identity policy)
  -> phase 10 operations and scale
```

Work inside a phase may proceed in parallel once its contracts are stable. For example, documentation can be drafted alongside implementation, and the operator console surfaces for a capability can land in the same change as the capability. Authorization, Git storage, and transport work must keep the order above, because each builds on the previous security boundary.

## Immediate next work

Phase 0 and phase 1 are complete. The phase-0 list that stood here is kept only as the record of the order those workstreams were built in.

Phase 1 is complete and its exit criteria are met. Phase 2 is under way: **W2.1, W2.2a–c, W2.3, W2.4a/b, and W2.5a are implemented.** The next work is **W2.6 (mentions, references, and the timeline)**, which is unblocked and does not depend on the assignee decision.

**One workstream is blocked on a decision rather than on work: W2.5b (assignees).** The parity line is `Multiple assignees limited to users with current read access`, and three things the documents do not answer each change what a page shows and what an audit record must say:

1. **When "current read access" is checked** — at assignment, at every read, or both?
2. **What happens to an existing assignment when the assignee's access is later revoked** — kept and hidden, removed, or retained and disclosed? `secrets.md`'s "a revoked credential fails on the next request" is about credentials rather than about a stored relationship, so it does not settle it.
3. **Whether an account with no repository access may be assigned at all.** GitHub allows only collaborators, which is *narrower* than the parity line's wording — so this would make the line wrong rather than the code.

The decision also fixes what a template's `assignees:` key may do, because W2.5a carries that metadata through.

**The status-mapping pattern is now closed rather than merely noted.** After the omission happened twice — the comment domain, then the label domain — W2.4b added the milestone domain to **both** `api.statusFor` and `web.statusForFormError` in the same change, and a test asserts the two agree on the statuses that matter. A future domain should follow that, and the two mappings are the pair to check.

**A pattern worth naming, because it has now happened twice.** The web package's `statusForFormError` and the API's `statusFor` are two mappings of the *same* domain errors to statuses, and each time a new domain is added the web mapping has been forgotten: the comment domain first, then the label domain. The symptom is always the same — a deliberate refusal answered as `422` where the API answers `409` or `403` — and only an end-to-end test finds it, because a domain unit test exercises the service rather than the transport. **The next domain should add both mappings in the same commit**, and the two are asserted to agree by a test on the pairs that matter.

**How W2.2c changed the plan, and why that is recorded rather than silently done.** The previous round's plan named a React feature as the next step, on the assumption that the interactive bundle was where a browser action belonged. Reading `docs/architecture/frontend.md` decided otherwise: it requires that **"A page must be usable before hydration. The React islands enhance a working page; they do not build it"**, and the issue detail page is already server-rendered and already owns its path. So the mutation controls are server-rendered forms. The consequences are that every issue operation works with scripting disabled, and — the reason this matters for the plan — **the workstream is verifiable in the development environment**, whereas a React feature could not have been. A future workstream should apply the same reading: a control that changes something on an *existing server-rendered page* is a form; a React island is for a surface the documents assign to the client.

**The issue-edit contradiction is resolved, and W2.3 is what resolved it.** The parity line `Edit issue title and body with an append-only revision record` appeared to contradict `docs/reference/events.md`, which defines no `issue.edited`. Building W2.3 showed the two are **consistent**, because the "revision record" is a domain table and the catalogue governs the audit ledger:

- **The audit ledger** is the security record — who acted, on what, when, from where — and has no foreign key to the content it describes, which is what makes it outlive that content. `comment.deleted` is the only comment action the catalogue defines.
- **A revision** is domain data: the previous text, attributed and ordered, which the UI shows as "edited". It belongs to the content and cascades with it. `comment_revisions` is that table for comments.

So an edit appends a revision and writes no ledger record, which is what W2.3 does and what its tests assert. The issue parity line therefore needs a **`issue_revisions` table**, not a catalogue action — the same mechanism applied in phase 3. **No maintainer decision is required**, and this replaces the question raised in the previous round.

**Two things that do need a maintainer, both raised by W2.3:**

1. **The flat `GET /issues/comments/{comment_id}` path that `urls-and-identifiers.md` §3 lists is unregistered.** With no issue in the path, a comment number must be unique across the repository, which needs either a repository-scoped comment counter (a schema decision) or a global per-instance identifier (what GitHub uses). Both are contract decisions, so the path is not invented — see the note in the comment API.
2. **`W2.2`'s issue edit still has no revision table.** W2.3 establishes the mechanism; applying it to issues means a second migration and a second history on the issue page. It is a small, well-specified change rather than an open question, and it belongs with whichever workstream next touches issues.

Notes for whoever picks up W2.2b and the workstreams after it:

- **The Markdown renderer is the only path by which user text becomes HTML.** An issue body, a comment, and a notification email must call `markdown.Renderer` rather than adding a second renderer or injecting a value into a template unescaped. `Render` returns `template.HTML`, which is safe **only** because the sanitizer ran and **only** as element content — never in an attribute, a URL, or a script.
- **There is one renderer per process, and the two modes differ.** `markdown.New()` is for a standalone document; `markdown.NewEmbedded()` demotes headings one level, for a document that appears inside a page which already has an `h1`. Both are cached separately, and the mode is part of the cache key.
- **The phase-2 schema does not exist yet.** `docs/architecture/data-model.md` puts `issues`, `issue_comments`, `labels`, `milestones`, `assignments`, `reactions`, and `timeline_events` in the Collaboration group, so W2.2 starts with a migration.

Five gaps need a maintainer decision rather than more code:

- **Resolved:** no workstream delivered a personal access token, which blocked phase 1's exit criteria. W1.14 now delivers the minimum a pusher needs, and the rest of the model stays with W4.8.
- **The credential model is phase 4 (W4.8).** Until it exists, every Git transport caller resolves as anonymous.
- **Two repository settings have no auditable type in the catalogue.** A name change needs the redirect the old path requires, and a description change needs an audit type that `docs/reference/events.md` does not define. Neither is reachable through the settings surface, and both need a decision before they are.
- **`GITNEST_SECRET_KEY` is documented as mandatory and is not implemented — with a proposal to defer it.** `docs/architecture/secrets.md` says the process refuses to start without it, but the same document defines its purpose: "A value that the server must replay is encrypted", listing OAuth client secrets, App private keys, webhook signing secrets, SMTP passwords, object-storage secrets, SAML keys, CI secret values, and workspace secrets. **None of those exist in phase 1**, so requiring the key now would encrypt nothing while adding a mandatory step to every deployment, a value in every test, and a value that is documented as unrecoverable if lost. The proposal is to introduce it in the workstream that first stores a replayable secret. It is deliberately **not** the cause of any CSRF limitation: a CSRF token is only ever compared, so `secrets.md` requires it hashed, not encrypted, and it is stored as a SHA-256 digest.
- **LFS has no table in the data model, and its quota is inexact because of it.** `docs/architecture/data-model.md`'s core entity map lists no LFS group, while `architecture.md` describes the object store, `urls-and-identifiers.md` gives the LFS routes as "Git LFS (phase 1)", and the parity line is P1. Both halves are now implemented with the **disk as the authoritative store**, which needs no table: an object's content is its digest, so existence is a direct path lookup rather than a walk. The outstanding consequence is `performance.md`'s `GIT-LFS-QUOTA`, specified as checked "under a transaction lock" — a per-repository counter is what that lock would be held against, and without it the quota is enforced against the bytes on disk, which two concurrent uploads can both pass. A maintainer should decide whether a counter table is added or the quota stays best-effort and documented as such.

The phase-0 order, for reference: W0.1–W0.7 built the service foundation (scaffolding, configuration, PostgreSQL connectivity, the migration runner, health and readiness, structured logging, bounded shutdown); W0.8–W0.10 added the local database recipe, the integration test harness, and the CI pipeline.

## Progress tracking

Three records must agree at the end of every round of work:

1. The phase table in "Current status" and the workstream statuses for the active phase in this file.
2. The `- [x]` state of [parity-checklist.md](parity-checklist.md).
3. The code in the working tree.

A phase is marked implemented only when its exit criteria are met and verified; a phase with a working subset is "in progress" or "partly implemented", never "implemented".

**A parity box is ticked only by the change that finishes the capability**, once its roadmap acceptance criteria pass with evidence. Never tick ahead of the work, never tick a partly finished item, never tick without evidence to show, and never leave a finished item unticked. If a ticked capability regresses, untick it in the change that reveals the regression. The CI `docs-check` job enforces the mechanical part of this rule: a ticked box whose phase has no corresponding implemented workstream fails the build.

When a decision changes, update [product-goals.md](product-goals.md) and [architecture.md](architecture.md) together with this plan and the affected design document under `docs/architecture/`. Keep implemented work distinct from planned work in every document.

## Re-plan checkpoint

At the end of each phase, before starting the next:

1. Compare the phase's actual duration against the estimate above and record the difference; adjust later estimates only with a stated reason.
2. Re-read the next phase's roadmap against the code that now exists and correct any workstream that assumed an interface that changed.
3. Confirm the parity checklist reflects what landed and what was deliberately excluded.
4. Confirm every cross-cutting requirement still holds for the code that was added.

## Working agreements

- Keep a change focused on one user-visible behaviour or one operational capability.
- Add a migration with every schema change, following the expand-and-contract rules in [architecture/data-model.md](architecture/data-model.md). Never edit a migration that may already have been applied.
- Treat Git processes, Git data, Markdown, diffs, uploaded files, package uploads, workflow definitions, and untrusted job output as hostile input.
- Add tests at permission and storage boundaries, not only around HTTP status codes. Every permission-bearing feature tests allowed and denied paths.
- Prefer the Go standard library and established libraries at real protocol or infrastructure boundaries. Avoid adding a framework for a single route or helper.
- Do not create empty packages for future phases. Add domain code when its phase starts.
- Keep parity gaps visible: a deliberately unimplemented GitHub behaviour is recorded in the relevant roadmap file's "Out of scope" section and in [parity-checklist.md](parity-checklist.md).
- Never let a transport answer an authorization question itself.
- Never let untrusted code run in the web process or receive platform credentials.
- Update the README when commands, configuration, or upgrade behaviour change.

## Long-term design constraints

- Preserve normal Git repository formats and provide export paths for metadata and repository data.
- Keep stable internal IDs separate from names, URLs, and filesystem paths.
- Keep authorization in shared application services so new transports and integrations cannot bypass it.
- Version public APIs and event payloads before external users depend on them.
- Make background jobs durable and idempotent before adding work that cannot finish in a request.
- Treat optional infrastructure as optional: a small self-hosted installation must not need a broker, a search cluster, or a container orchestrator.
- Document upgrade, restore, and downgrade limits for every schema or storage change.
