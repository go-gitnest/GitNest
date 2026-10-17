# Glossary

**Status:** Target vocabulary. This document fixes the words the roadmap, the architecture,
and the code use, so a term does not drift between phases. A definition describes the design
GitNest is building, not implemented behaviour.

**How to use it:** When a document, a type name, or a UI label needs one of these concepts, use
the term here and the meaning here. If a phase needs a new term, add it here in the same
change. "GitHub equivalence" states the compatibility requirement in a few words: what must
match for an existing GitHub habit, script, or client to keep working.

Related: [product-goals.md](product-goals.md) (parity definition and exclusions),
[architecture.md](architecture.md) (modules, trust boundaries),
[architecture/urls-and-identifiers.md](architecture/urls-and-identifiers.md) (routes, naming,
numbering), [implementation-plan.md](implementation-plan.md) (phase order).

## Accounts, ownership, and access

| Term | Definition | GitHub equivalence |
| --- | --- | --- |
| **Account** | The identity record behind a user, organization, enterprise, or machine user. It owns credentials, sessions, and grants, and its UUID is the stable internal identity that never appears in a URL. | Accounts replace nothing; they are the shared substrate GitHub splits across user, organization, and enterprise types. |
| **User** | A person's account: a login, display name, profile, credentials, sessions, tokens, SSH and GPG keys, and authorship of content. A user may own repositories and belong to organizations. | Same noun and same login-based URL `/{login}`. |
| **Organization** | A shared account that owns repositories, teams, members, and projects, with organization-scoped roles, policies, and later SSO and provisioning. | Same noun; organization login occupies the same URL segment as a user login. |
| **Enterprise** | A container above organizations, with member organizations, enterprise owners, and policies that organizations can be required to follow. It exists so administrative requirements can be set once rather than per organization. | Enterprise account with policy inheritance; billing, seat licensing, and metering are excluded. |
| **Team** | A named group inside one organization, optionally nested under a parent team, used to grant repository access and to request reviews. Membership is implicit access to everything the team is granted. | Same noun, same slug-based routes, same nested-team inheritance. |
| **Organization role** | One of `owner` and `member` — the named position a user holds in an organization. Roles are coarse and are the thing a UI shows. | Same role names for the same scopes. |
| **Outside collaborator** | A user who is not an organization member but holds a direct grant on one or more of the organization's repositories. The grant is exactly the granted role and carries no organization-wide visibility. | Same term, same confinement to granted repositories. |
| **Role versus permission** | A **role** is a named bundle assigned to a principal (organization owner, member; repository owner, admin, write, read; team grant role). A **permission** is the resolved answer to "may this principal do this verb on this resource", computed by the shared authorization service from roles, grants, visibility, token scopes, app permissions, and policy ceilings. Roles are stored; permissions are derived and are never stored as duplicated truth. | GitHub's role vocabulary (`admin`, `write`, `read`, `triage`, `maintain`, `owner`) resolves to the same effective decisions. |
| **Visibility** | Whether a repository (or package, or workspace) is readable by an unauthenticated caller: `public` or `private`. Private is the default for a new repository. Visibility governs read only; every write requires an authenticated principal with the required permission. | `public` / `private`, private by default, matching GitHub's create-repository default. |
| **Collaborator** | A principal with an explicit grant on a repository, either a user (direct collaborator, including an outside collaborator) or a team (through an organization). A collaborator is a grant, not a role by itself. | Same term, same additive resolution across direct and team grants. |
| **Fork** | A repository created as a copy of another repository, recorded with its upstream relationship, that can host branches and receive pull requests into the upstream. | Same noun and same upstream/head comparison behaviour. |
| **Deploy key** | An SSH public key granted read-only or read-write access to exactly one repository. GitNest stores only the public key and its fingerprint; the private key never reaches the server. | Same noun, same one-repository scope, same read-only/read-write choice. |
| **Machine user** | An organization-owned account that exists for automation: it cannot sign in interactively, cannot consent to OAuth, and has no session. Exemption from interactive controls is by account kind, not by name. | Same concept as a GitHub machine user account. |
| **Principal** | The authenticated actor behind a request: a user session, a personal access token, an OAuth app authorization, a GitHub App installation, a machine user, a deploy key, a run-scoped workflow token, or a workspace session token. Every transport resolves its caller to a principal before asking the authorization service. | Matches GitHub's distinction between user, app, and installation authentication. |

## Credentials and integrations

| Term | Definition | GitHub equivalence |
| --- | --- | --- |
| **Personal access token (classic)** | A user-owned bearer token with a fixed set of scope strings (`repo`, `read:org`, `workflow`, `packages`, and so on), an optional expiry, and a single revocation path. Shown once at creation, stored hashed with a short display prefix. | Same scope names and the same `Authorization: token` / `Bearer` acceptance. |
| **Fine-grained token** | A user-owned token that selects a resource scope (all repositories, selected repositories, or one owner) plus an explicit per-resource permission set, with expiry and revocation. Granted least privilege by construction. | Same resource-and-permission selection model. |
| **OAuth app** | A third-party application registered with a client ID, a hashed client secret, registered callback URLs, and requested scopes, which acts **as the authorizing user** after an authorization-code flow with required single-use `state` and PKCE for public clients. | Same authorization-code flow, same `state` requirement, same exact-match redirect validation. |
| **GitHub App equivalent** | A first-class application registered on the instance with its own identity, a PEM private key, permissions it declares up front, and per-installation scoped access. It acts as itself, not as a user. Marketplace listing, distribution, and payments are excluded. | The GitHub Apps framework, registration, JWT authentication, and permission model. |
| **Installation** | One app installed on one account (user or organization), with a repository selection and a permission set. It authenticates with a short-lived installation token scoped to that installation and its selected repositories, and can be suspended or uninstalled. | Same installation model, same token lifetime bound, same suspend/uninstall lifecycle. |
| **Installation token** | The access token an app mints for one installation, expiring within an hour, scoped to the installation's repositories and permissions, and individually revocable. | Same one-hour class of token scoped to the installation. |
| **Webhook** | A subscription owned by a repository, organization, or app that names a target URL, an event list, a content type, and a secret, and receives signed event payloads for matching activity. | Same subscription model, same `X-Hub-Signature-256` HMAC-SHA256 signing over the raw body. |
| **Delivery** | One attempt to send one event payload to one webhook subscription, recorded with request headers, a body digest, response status, a bounded response prefix, duration, attempt number, and a delivery GUID. Deliveries are retried with backoff on transport and `5xx` failures, never on `4xx`, and can be redelivered byte-identically. | Same delivery record and redelivery semantics; the delivery GUID identifies the attempt. |
| **Check run** | The result of one named check against one commit: status, conclusion, a details URL, and bounded output and annotations, attributed to a head object ID so a result for an older commit cannot satisfy a current requirement. | Same `check_run` resource and head-SHA attribution. |
| **Check suite** | The grouping of check runs produced by one workflow run or one app for a commit. Used to present "all checks for this commit" as one unit and to derive aggregate status. | Same `check_suite` grouping. |
| **Required status check** | A named check (optionally bound to a specific app) that branch protection or a ruleset requires on the head commit before a pull request may merge, with a documented rule for which result is authoritative when a name is reported twice. | Same required-check configuration and merge gate. |
| **Branch protection** | A per-branch (and per-pattern) legacy rule set: block force pushes, block deletion, require a pull request, required approving reviews, dismiss stale approvals, require CODEOWNERS review, require conversation resolution, require linear history, require signed commits, and restrict who may push. Enforced in the merge service and the Git receive path. | Same rule names, same enforcement points. |
| **Ruleset** | A named, ordered, targeted rule collection with an enforcement status (`active`, `evaluate`, `disabled`), a bypass list, and an optional repository scope, definable at repository or organization level, where organization rulesets can be required so a repository owner cannot weaken them and the most restrictive applicable rule wins. | Same ruleset model including `evaluate` mode and required organization rulesets. |
| **CODEOWNERS** | A repository file that maps path patterns to users or teams, resolved at evaluation time, whose owners are requested as reviewers and can be required to approve. Files are read from `.github/CODEOWNERS`, `CODEOWNERS`, then `docs/CODEOWNERS`, first match wins. | Same file locations, same first-match precedence, same review-request behaviour. |

## Actions and delivery automation

| Term | Definition | GitHub equivalence |
| --- | --- | --- |
| **Workflow** | A YAML file under `.github/workflows/` at a ref, defining triggers, jobs, steps, permissions, and concurrency. It is read at the triggering ref, stored with the exact commit SHA it came from, and immutable for the run it produced. | Same discovery path, same file schema and trigger names, so an existing workflow directory runs unedited. |
| **Workflow run** | One execution of a workflow definition for one event and one head commit, with an outcome, a log, a resolved action list, and a stored definition revision. Cancelling and rerunning create new runs linked to the original rather than mutating it. | Same `workflow_run` noun, same queued/in-progress/completed status model. |
| **Job** | One unit of work inside a workflow run, defined by a job ID, a runner selection, a matrix payload, dependencies, and a sequence of steps. Jobs claim a lease from the durable queue and can run concurrently within instance limits. | Same job granularity, same `needs` graph semantics. |
| **Step** | One task inside a job: either a `run` command with a shell, or a `uses` reference to an action. Steps have a condition, a timeout, an exit-code outcome, and captured outputs. | Same step model and output-passing files (`GITHUB_OUTPUT`, `GITHUB_ENV`, `GITHUB_PATH`). |
| **Action** | A reusable unit invoked by `uses`: a JavaScript action, a Docker action, or a composite action, resolved from a local path or a remote reference pinned by full commit SHA. | Same action kinds and the same SHA-pinning requirement; the Marketplace and its payments are excluded. |
| **Reusable workflow** | A whole workflow invoked from another workflow with `uses: ./.github/workflows/x.yml`, declaring `workflow_call` inputs, outputs, and secrets. Nesting depth is bounded and recursion is rejected. | Same reusable-workflow call semantics, including `secrets: inherit`. |
| **Composite action** | An action whose implementation is a nested list of steps, sharing the caller's job environment. | Same composite-action semantics. |
| **Matrix** | A job strategy that expands one job definition into many jobs over declared value combinations, with `include` and `exclude`, expanded at run creation under a hard cap. Matrix values may not be sourced from a secret. | Same matrix expansion semantics and caps. |
| **Concurrency group** | A named lease, scoped to a repository, that allows at most one run in the group to execute and at most one to be pending, with `cancel-in-progress` deciding whether the pending run supersedes the running one. | Same `concurrency` semantics, including `cancel-in-progress`. |
| **Runner** | The component that leases a job from the queue, executes it in an isolated environment outside the web process, streams logs, and uploads artifacts and caches. It never holds a credential that reaches GitNest's database. | Same role as a GitHub runner. |
| **Self-hosted runner** | An operator- or organization-registered runner with labels, registered with a single-use, expiring, scoped registration token, and matched to jobs by label so a job with no matching label stays queued. | Same label matching and scoped registration tokens. |
| **Artifact** | A named file set uploaded by a job and downloadable after the run, with per-artifact retention days, a size cap, and deletion when the run is deleted. | Same artifact noun, same retention behaviour. |
| **Cache** | A blob store keyed by an exact key plus prefix `restore-keys`, scoped by branch, bounded per repository and per instance, and evicted LRU. A cache written on a protected branch is not readable from a fork pull request. | Same key/restore-key and branch-scoping semantics. |
| **Secret** | An encrypted-at-rest value injected into a job's environment at run time, registered with the log masker, never returned by any API after creation, and never persisted into an image, a cache, or a log. Fork pull requests receive no repository secrets. | Same secret injection and masking behaviour. |
| **Variable** | A non-secret configuration value at repository, organization, or environment scope, readable by workflows through the `vars` context and returnable through the API. Identical in scope model to a secret, without encryption or masking. | Same `vars` context and scoping. |
| **Environment** | A named deployment target inside a repository with protection rules (required reviewers, wait timer, branch and tag restrictions, required checks) and its own secret and variable scope. A deployment is created pending and must pass protection before the job starts. | Same environment noun and required-reviewer gating. |
| **Deployment protection rule** | One constraint attached to an environment: a required reviewer set, a wait timer, a branch or tag restriction, or a required check. Evaluated by the shared service before any secret is injected or runner is scheduled, with an audited approval trail. | Same protection-rule kinds and approval semantics. |
| **Workflow token** | The per-run token (`GITHUB_TOKEN` equivalent) minted with a read-only default permission set, widened only by the workflow's `permissions` block, scoped to the originating repository, and expiring at run end. | Same read-only default and explicit widening model. |

## Repositories, content, and delivery

| Term | Definition | GitHub equivalence |
| --- | --- | --- |
| **Repository** | A bare Git repository plus its metadata: owner, name, visibility, default branch, permissions, features, and the canonical storage ID that addresses the bytes. It is the unit that most permissions, content surfaces, and integrations hang from. | Same noun, same `{owner}/{repo}` addressing, same standard bare-repository format. |
| **Default branch** | The ref used when a request names no ref, when a pull request targets the main line, and when most content surfaces read code. Renaming it is a repository setting, and the old ref keeps resolving while it exists. | Same role and same rename behaviour. |
| **Branch** | A movable ref under `refs/heads/` in the repository. Addressed by name in URLs, protected by branch protection or a ruleset, and pushed over HTTPS or SSH. | Same ref semantics; protected-ref rules enforced on push and merge. |
| **Tag** | A ref under `refs/tags/`, lightweight or annotated. A release binds to a tag name and records the exact commit object ID so a later tag move does not change what the release describes. | Same tag semantics and the same release-to-commit binding. |
| **Commit-addressed route** | A route whose target is an immutable commit object ID (a full 40-hex SHA) rather than a movable ref. It is the citation form used in check links, review anchors, and payloads, while branch and tag routes are the human-facing form. | Same `/commit/{sha}` immutability and the same interchangeable ref/SHA routing. |
| **Content-addressed route** | A route whose target is a digest: a Git object ID for a blob or tree, or a `sha256:` digest for a package blob. The digest is the identity; the content is immutable and cacheable for a long time. | Same blob-by-sha and registry digest addressing. |
| **Wiki** | A per-repository documentation surface stored as its own bare Git repository beside the code repository, edited in the browser through the same bounded Git path, with a sidebar and page history. | Same per-repository wiki backed by a separate Git repository. |
| **Pages** | Static site hosting for a repository: a build step that produces an immutable versioned artifact, an atomic publish pointer, same-origin serving by default, and optional verified custom domains with TLS. | Same publish-by-build model and custom-domain verification; GitHub's own hosting infrastructure is not reproduced. |
| **Release** | A published point in a repository bound to a tag and a commit object ID, with a title, notes (optionally generated from merged pull requests and commits), a draft state, a prerelease flag, and a derived `latest` marker. | Same release noun, same draft/prerelease/`latest` semantics. |
| **Release asset** | A file attached to a release with a name, a content type, a size, an upload cap, and a range-capable download. Re-uploading the same name replaces the asset rather than creating a duplicate. | Same asset model, same replacement and entitlement behaviour. |
| **Package** | A named publishable artifact owned by a repository or organization, in one registry kind (OCI, npm, Maven, NuGet, RubyGems, Composer, Helm, generic), whose visibility is inherited from its owner rather than set separately. | Same package noun and inherited visibility. |
| **Package version** | One immutable published version of a package, with metadata, a digest set, download accounting, and an optional yank. Blobs are content-addressed and reference-counted so a version delete is a metadata operation followed by a blob sweep. | Same version immutability and yank behaviour per ecosystem. |
| **Registry kind** | The protocol family a package is published through (OCI, npm, Maven, NuGet, RubyGems, Composer, Helm, generic). The same package name in two registry kinds does not collide. | Same registry-per-ecosystem model. |

## Planning, conversation, and discovery

| Term | Definition | GitHub equivalence |
| --- | --- | --- |
| **Issue** | A repository-scoped work item with a number from the shared per-repository sequence, a title, a body, a state, assignees, labels, and a milestone, discussed through comments on a unified timeline. | Same issue noun, same shared numbering with pull requests. |
| **Pull request** | A proposed change from one ref to another (same repository or a fork), with a diff, review threads, review states, mergeability, and a merge that updates the target ref with compare-and-swap safety. It shares the issue number sequence. | Same pull-request noun and the same `#number` shared with issues. |
| **Project** | A planning surface scoped to a repository, user, or organization, shown as a board, table, or roadmap over the same items, with custom fields (text, number, date, single select, iteration), per-view grouping, sorting, and filtering, and bounded audited automations. | Same project noun and view/field model. |
| **Project field** | A typed custom attribute defined on a project and set per item, validated on write by type, with a documented set of types and rejection of unknown select options or out-of-range values. | Same custom-field types and validation. |
| **Discussion** | A repository-scoped conversation in a category with a format (open discussion, question and answer, announcement), threaded replies, upvotes, polls, and, for Q&A, a single markable answer. | Same discussion noun, categories, and Q&A answer marking. |
| **Answer** | The single reply marked as resolving a question-and-answer discussion. Re-marking moves the answer and is audited; only one answer exists per discussion at a time. | Same single-answer rule for Q&A discussions. |
| **Search index** | The queryable projection of repositories, code, commits, issues, pull requests, users, and topics behind one search port. The baseline implementation is PostgreSQL full-text search with a documented degradation story; an external service is an optional adapter with a rebuildable index and an explicit completeness signal. | Same qualifier vocabulary and result types; the index engine is GitNest's own choice. |
| **Topic** | A normalized, unique slug with a display name and description used to categorize repositories, manageable by a user with admin permission, with operator-curated featured topics. | Same topic slug model and topic pages. |
| **Star** | A per-user, idempotent marker of interest in a repository that feeds listing, ranking, and feeds. Starring never grants or implies read access to a private repository. | Same star semantics and counts. |
| **Watch / subscription** | The per-user state that decides which activity produces notifications for a thread or repository: `participating`, `all activity`, `ignore`, or muted. The same subscription concept backs thread-level notification preferences. | Same participating/all-activity/ignore modes and mute. |
| **Notification thread** | A grouped set of notifications about one subject (an issue or pull request) with read/unread state, a reason, and a per-thread subscription. Fan-out is a durable, idempotent job so an event fanned out twice produces one user-visible notification. | Same thread grouping and reason vocabulary. |
| **Activity event** | An append-only record of a domain change (actor, verb, target, scope, timestamp) written in the same transaction as the change, used for repository and organization feeds. | Same activity-versus-audit split. |
| **Audit event** | An append-only, immutable administrative record at organization or enterprise scope covering credential, membership, visibility, protection, approval, deletion, and policy changes, queryable with filters and exportable as a signed, tamper-evident file. | Same audit-log coverage and immutability. |

## Cloud development

| Term | Definition | GitHub equivalence |
| --- | --- | --- |
| **Workspace** | A cloud-development environment for one user, created from a repository and ref or with no repository, running untrusted code in an isolated container outside the web process, with a browser IDE, a terminal, forwarded ports, and a persistent volume. Workspaces replace GitHub Codespaces and Gist-style scratch spaces. | The Codespaces user experience; GitHub's own service and billing are excluded. |
| **Devcontainer** | The `.devcontainer/devcontainer.json` (or root `devcontainer.json`) configuration that defines a workspace's image or build context, features pinned by version, user, environment, mounts, forwarded ports, lifecycle commands, and workspace folder. Parsed strictly, with mounts constrained to GitNest-controlled roots. | Same devcontainer configuration file and keys, so an existing configuration works unchanged. |
| **Prebuild** | An image plus volume snapshot built ahead of time from a configured branch, keyed by repository revision and devcontainer content hash, reused by a compatible workspace creation and pruned by age and count. | Same prebuild concept and compatibility-by-content-hash reuse. |
| **Snapshot** | A named recording of a workspace filesystem (image plus volume snapshot) with creator and size, restorable into the same or a new workspace, expiring by policy with released quota accounting. | Same snapshot and restore semantics. |
| **Workspace share** | An explicit grant of `read`, `edit`, or `terminal` access on one workspace, delivered as a short-lived invitation by the owner or a repository admin, plus an optional signed, expiring link share. Sharing is never implied by repository access alone. | Same live-share and link-share model. |
| **Preview URL** | The per-workspace URL that proxies one forwarded port, resolved from a signed route or a stored host mapping, never from a client-supplied address, and access-controlled with a `404` for an unauthorized caller so port existence is not disclosed. | The Codespaces forwarded-port URL behaviour. |
| **Quota** | An enforced limit on a resource — concurrent workspaces, CPU, memory, disk, lifetime hours, Actions job minutes, artifact bytes, cache bytes, or package bytes — scoped to a user, repository, organization, or the instance, whose rejection names the exhausted resource. | Same idea as GitHub's usage limits, without billing or seat licensing. |
| **Scratch space** | A workspace with no repository attached, private by default, with an owner-scoped volume and an explicit export that creates a new GitNest repository from its contents. It is the replacement for Gist. | Gist's scratch-pad use case as a workspace; Gist as a separate product is excluded. |

## Terms deliberately not used

| Avoid | Use instead | Why |
| --- | --- | --- |
| "public repo count" as a ranking input without a stated window | trending score with a documented rolling window | Ranking inputs must be reproducible and documented. |
| "permission" for a stored field | role (stored) / permission (derived) | A stored permission duplicates the authorization service. |
| "owner" for a team or a token | owner (a user or organization) for repositories; grant for anything else | Ownership is an account relationship, not a grant. |
| "branch protection" for an organization-wide rule | ruleset (organization-scoped, required or not) | Protection is per repository; organization policy is a ruleset or an enterprise policy. |
| "Codespace" as a GitNest product noun | workspace | The product is GitNest's own; Codespaces is the excluded reference. |
| "Gist" for a scratch pad | scratch workspace | Gist is a decided exclusion. |
| "security scanning", "secret scanning", "Dependabot" | Advanced Security (excluded) | These are decided non-goals and must not appear as planned surfaces. |
