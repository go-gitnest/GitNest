# Product goals

**Status:** Positioning revised. GitNest is now scoped as a self-hosted Git forge targeting feature parity with GitHub, delivered in phases.

**Companion documents:** the auditable capability inventory is [parity-checklist.md](parity-checklist.md); shared vocabulary is in [glossary.md](glossary.md); cross-cutting design is under [architecture/](architecture/) and [security/threat-model.md](security/threat-model.md); event catalogues are in [reference/events.md](reference/events.md).

## What GitNest is for

GitNest is a self-hosted home for Git repositories and all the work around them, built to match what a team already expects from GitHub. An organization installs one instance on its own infrastructure, keeps its code and metadata under its own control, and uses a browser plus ordinary Git clients for daily work: repositories, issues, pull requests, review, CI, packages, releases, search, and administration.

GitHub is the reference implementation. GitNest reproduces its concepts, workflows, permission model, and API shapes rather than inventing its own. A developer who moves from GitHub to GitNest should not have to relearn the product, and an existing script or CI job that talks to the GitHub API should need as little change as possible.

GitNest is not a Git implementation. It stores standard bare repositories and invokes the installed Git executable. A user can always leave with a normal clone that works everywhere else.

## What "parity" means here

Parity is a behavioural commitment, not a visual one:

- **Same nouns and workflows.** Repositories, issues, pull requests, reviews, actions, packages, releases, organizations, teams, projects, discussions, wikis, pages.
- **Same permission model.** Owner/admin/write/read, public and private visibility, organization and team inheritance, branch protection and rulesets.
- **API compatibility where practical.** REST resource shapes, pagination, conditional requests, rate-limit headers, error bodies, and a versioned API surface, plus GraphQL, so existing tooling keeps working.
- **Absolute Git compatibility.** Smart HTTP and SSH, protocol v2, LFS, archive downloads, and no proprietary repository representation.
- **Not pixel parity.** The UI is GitNest's own, but every workflow GitHub offers has a browser path here.

Parity is delivered in phases. Each phase ends with a deployable instance; no phase leaves the product in a state an operator cannot run.

## Explicit exclusions

These are decided non-goals. They must not appear as workstreams in the roadmap.

| Excluded | Reason |
| --- | --- |
| Mobile application | Out of scope by decision; the API and web UI are the supported clients |
| GitHub Codespaces itself | Superseded by GitNest's own cloud-development subsystem (phase 9) |
| Advanced Security: code scanning / CodeQL, secret scanning, Dependabot alerts and security updates | Out of scope by decision |
| Security advisories and the vulnerability database | Adjacent to Advanced Security, which is excluded; no advisory workflow is planned |
| Gist | Superseded by cloud-development scratch spaces (phase 9) |
| Sponsors, payments, billing, marketplace payouts | GitNest is not a billing platform |
| Copilot and AI code features | Out of scope by decision |
| Advertising, activity feed as a social product | Not part of a self-hosted forge |
| Federation between instances | No interoperable contract exists to target |
| Kubernetes operators, Helm charts, and managed cloud offerings | The supported deployment is a binary, a container image, and a system service; optional infrastructure must not require an orchestrator |
| Cross-region active-active replication | A single instance with a coordinated backup is the supported availability model; multi-node scaling is phase 10 |
| Management consoles beyond the operator console defined in [architecture/administration.md](architecture/administration.md) | The operator console covers instance administration |

The exclusions are mirrored line by line in [parity-checklist.md](parity-checklist.md), which is where the unticked remainder of the parity surface stays visible. Moving a capability into or out of this table is a product decision and is recorded here first.

Two clarifications that are easy to get wrong:

- **GitHub Apps are in scope** even though Marketplace payments and distribution are not. The app framework, installations, permissions, installation tokens, and per-app webhooks are part of parity.
- **Branch protection, required status checks, audit logs, and SSO are in scope.** They are governance features, not Advanced Security products.

## Who it serves

- **Teams and organizations** that want their forge on their own hardware, from a handful of developers to an internal instance with several thousand accounts.
- **Operators** who install, upgrade, back up, and troubleshoot the instance. They are comfortable with PostgreSQL, persistent storage, a reverse proxy, and containers, but should not need a Kubernetes cluster for the common case.
- **Automation and integrators** that drive the platform through the REST API, GraphQL, webhooks, GitHub Apps, and Actions.

## Feature surface

The parity inventory, grouped by the phase that delivers it. Each area has a roadmap file with its workstreams and acceptance criteria.

| Area | Phase | Roadmap |
| --- | --- | --- |
| Identity, accounts, repository hosting, Git over HTTPS, backup | 1 | [01-identity-and-repository-hosting.md](roadmap/01-identity-and-repository-hosting.md) |
| Issues, organizations, teams, notifications, SSH transport | 2 | [02-collaboration.md](roadmap/02-collaboration.md) |
| Pull requests, review, merge | 3 | [03-pull-requests.md](roadmap/03-pull-requests.md) |
| REST, GraphQL, tokens, OAuth and GitHub Apps, webhooks | 4 | [04-api-and-extensibility.md](roadmap/04-api-and-extensibility.md) |
| Actions: workflows, runners, secrets, artifacts, caches | 5 | [05-actions.md](roadmap/05-actions.md) |
| Packages and releases | 6 | [06-packages-and-releases.md](roadmap/06-packages-and-releases.md) |
| Search, topics, stars, wiki, pages, discussions, projects, insights | 7 | [07-discovery-and-surfaces.md](roadmap/07-discovery-and-surfaces.md) |
| Branch protection, rulesets, 2FA, SSO/SAML, SCIM, LDAP, audit, moderation | 8 | [08-governance-and-enterprise.md](roadmap/08-governance-and-enterprise.md) |
| Cloud development workspaces (Codespaces replacement) | 9 | [09-cloud-development.md](roadmap/09-cloud-development.md) |
| Multi-node, object storage, search service, observability, upgrades | 10 | [10-operations-and-scale.md](roadmap/10-operations-and-scale.md) |

The ordering is a dependency order, not a priority ranking. Later phases depend on the authorization, storage, and API contracts established earlier; see [implementation-plan.md](implementation-plan.md).

## Product rules

- **Private by default.** A new repository is private unless its owner selects public. Public repositories allow anonymous read only; writing always requires an authenticated account with write permission.
- **Accounts are administrator-controlled by default.** The instance bootstrap creates one administrator. Self-registration is an explicit instance setting that is off unless an operator turns it on, matching GitHub's configurable model.
- **One instance, many tenants.** Organizations and teams are first-class; the instance is not a single-team tool.
- **Ordinary Git behaviour.** No custom client, no server-side branch format, no proprietary pack handling.
- **Permissions are legible before the action.** The UI states visibility and the current user's permission on every page where an action is possible, and the API returns the same answer as the browser.
- **Everything consequential is auditable.** Membership, permission, visibility, credential, and repository-destructive changes produce audit events an operator can retrieve.
- **Data is recoverable and portable.** Repository data and PostgreSQL metadata have a documented, coordinated backup procedure, and metadata has an export path.
- **Secrets never reach logs.** Passwords, session and invitation tokens, personal access tokens, private keys, webhook secrets, action secrets, and repository contents are never written to logs, error messages, or metrics labels.

## Priorities and ordering principles

1. **Dependency order over feature appeal.** Authorization and storage contracts come before the features that depend on them.
2. **Push the security boundary outward deliberately.** Git transport, repository content, Markdown, and untrusted execution (Actions, cloud development) are introduced one at a time, each with its own threat model and tests.
3. **Every phase is deployable.** At any commit on the plan, an operator can install, upgrade, and back up the instance with the documented procedure.
4. **Parity gaps stay visible.** Untracked divergence is a defect. If a GitHub behaviour is deliberately not implemented, it is recorded in the relevant roadmap file's "Out of scope" section.
5. **No feature ships without its denied path.** Every permission-bearing feature carries tests proving both allowed and denied behaviour, at the service boundary rather than only at the HTTP status code.
6. **Heavy infrastructure is opt-in.** Object storage, a search service, a broker, multiple nodes, and a container runtime are introduced only by a workstream that requires them, and the single-node path keeps working.

## Definition of done for a feature

A feature is complete when all of the following hold:

- A browser flow exists and is keyboard-usable.
- The REST API exposes it (and GraphQL where the surface warrants it), with stable error bodies and pagination.
- Authorization is enforced by the shared application service, not by the transport.
- Tests cover the allowed and denied paths, plus persistence and concurrency behaviour where relevant.
- Schema changes ship as a new forward-only migration.
- Audit events are emitted for consequential changes.
- Operator-facing documentation is updated when configuration, commands, or upgrade behaviour change.
- No secret or repository content can reach a log line, error response, or metric label.

## Long-term direction

The long-term product is a self-hosted forge that is a credible replacement for GitHub for a real organization, without requiring GitHub's operational scale to run. Growth areas are additional parity depth, better scale-out options, and integrations, in that order. Each new area still needs an operator story for upgrades, data migration, backup, access control, and failure recovery before it becomes a release commitment.

## Long-term success

The project is succeeding when:

- A team can migrate its repositories, issues, pull requests, and CI definitions from GitHub and keep working without changing its habits.
- Existing GitHub API clients and Actions workflows run against GitNest with minimal or no modification.
- An operator can install, upgrade, and restore an instance from documented steps alone.
- A failure can be diagnosed from logs and metrics without reading source code.
- Repository data can leave GitNest in a standard form at any time.

## Quality bar

- The same access decision produces the same answer in the browser, REST API, GraphQL, HTTPS Git, and SSH Git paths.
- Git operations never interpolate user input into shell commands and never accept a filesystem path from a client.
- Untrusted execution (Actions jobs, cloud-development workspaces) never runs in the web process and never receives host or database credentials.
- The service fails clearly when PostgreSQL, repository storage, or required infrastructure is unavailable.
- Configuration, migration, upgrade, and backup instructions are usable by someone who did not write the code.
- Documentation distinguishes implemented behaviour from planned behaviour at all times.
