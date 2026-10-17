# URLs and identifiers

**Status:** Target design. Nothing in this document is implemented; it is the contract the
rebuild must satisfy. Where a rule reproduces GitHub behaviour, it is stated as a
compatibility requirement so an existing GitHub client, script, or habit keeps working.

**Scope:** The canonical web route shapes, the API path shapes and their mapping to web
routes, Git remote URL generation, identifier grammar and uniqueness, issue and pull-request
numbering, rename and transfer redirects, content- and commit-addressed routes, pagination and
sorting parameters, and the per-route authorization and cacheability table.

**Owner:** This document is the single owner of the URL and identifier contract. A domain
package, transport, or UI that invents a route shape outside it is a defect.

Related: [../product-goals.md](../product-goals.md) (parity definition, exclusions),
[../architecture.md](../architecture.md) (transports, public URL rule, authorization rule),
[../implementation-plan.md](../implementation-plan.md) (phase order),
[../glossary.md](../glossary.md) (vocabulary).

## 1. Global rules

| Rule | Requirement |
| --- | --- |
| Origin | Every absolute URL GitNest generates is built from the configured `GITNEST_PUBLIC_URL`. Request headers, `Host`, and `X-Forwarded-*` never determine an origin or a link target. |
| Path casing | Routes are matched with ASCII case folding for owner and repository segments, and case-sensitively for everything else (`tree/<ref>` refs, file paths, label names in query values). |
| Canonical form | A resource has exactly one canonical URL. Every alias redirects to it with `301 Moved Permanently`; no alias is itself linked to, emitted in a payload, or shown in the UI. |
| Trailing slash | A trailing slash is redirected to the canonical form without one, except the site root. |
| Query noise | Unknown query parameters are ignored, not rejected. Known parameters with invalid values return the documented validation error. |
| Identifiers in URLs | Only names, slugs, numbers, refs, and content digests appear in a URL. A stable internal UUID never appears in a URL, a redirect target, a storage path, or a user-visible payload. |
| Private resources | A caller without access receives the same response as for a resource that does not exist. Route shapes do not disclose existence, counts, or numbers. |
| Reserved prefixes | The first path segment must not be a reserved name (section 6.9) for owner-scoped routes; instance-scoped reservations win over any user, organization, or repository named the same. |
| Route registration | Routes are registered as method-aware `net/http` `ServeMux` patterns (`GET /{owner}/{repo}/issues/{number}`). Method mismatch returns `405` with an `Allow` header. |

Two names are user-facing and may change: the route identifier (owner/name/ref/number) and the
display name. The internal UUID never changes and never appears in either.

## 2. Canonical web routes

Routes are shown as path patterns. `{owner}` is a user or organization; `{repo}` is a
repository; `{number}` is the shared issue/pull-request number; `{ref}` is a branch, tag, or
commit-ish; `{sha}` is a full 40-hex commit object ID.

### 2.1 Site and account

| Route | Surface | Canonical? |
| --- | --- | --- |
| `/` | Signed-in dashboard; anonymous landing when signed out | yes |
| `/login`, `/logout`, `/signup`, `/password_reset` | Authentication surfaces | yes |
| `/settings/profile`, `/settings/appearance`, `/settings/notifications`, `/settings/emails`, `/settings/security`, `/settings/sessions`, `/settings/ssh`, `/settings/gpg`, `/settings/tokens`, `/settings/apps`, `/settings/installations`, `/settings/organizations`, `/settings/repositories`, `/settings/workspaces`, `/settings/dotfiles` | Account settings, one page per concern | yes |
| `/settings/security/two_factor` | Second-factor enrollment | yes |
| `/notifications`, `/notifications?query=...` | Notification inbox and filters | yes |
| `/explore`, `/topics`, `/topics/{topic}`, `/trending` | Discovery surfaces | yes |
| `/search?q=...&type=...` | Unified search | yes |
| `/workspaces`, `/workspaces/new`, `/workspaces/{workspace-id}`, `/workspaces/{workspace-id}/terminal` | Cloud-development surfaces (phase 9) | yes |
| `/organizations/new`, `/organizations/{org}/settings/...` | Organization creation and settings | yes |
| `/enterprises/{enterprise}/settings/...` | Enterprise administration (phase 8) | yes |

### 2.2 Profiles and user-scoped lists

| Route | Surface |
| --- | --- |
| `/{owner}` | Profile: repositories, overview, contribution summary |
| `/{owner}/{repo}` | Repository overview (README, about, latest activity) |
| `/{owner}?tab=repositories`, `?tab=stars`, `?tab=followers`, `?tab=following` | Profile tabs |
| `/{owner}/repositories`, `/{owner}/stars`, `/{owner}/followers`, `/{owner}/following` | Canonical list routes behind those tabs |
| `/orgs/{org}/repositories`, `/orgs/{org}/people`, `/orgs/{org}/teams`, `/orgs/{org}/projects`, `/orgs/{org}/packages`, `/orgs/{org}/insights` | Organization-scoped lists |

### 2.3 Code, history, and comparison

| Route | Surface | Canonical? |
| --- | --- | --- |
| `/{owner}/{repo}/tree/{ref}` | Tree at a ref; the commit form redirects here | yes |
| `/{owner}/{repo}/tree/{ref}/{path...}` | Tree rooted at a directory path | yes |
| `/{owner}/{repo}/blob/{ref}/{path...}` | File view (rendered, with source fallback) | yes |
| `/{owner}/{repo}/blob/{ref}/{path...}?plain=1` | Raw source view of the same file | yes |
| `/{owner}/{repo}/raw/{ref}/{path...}` | Byte-exact file body, `Content-Type: text/plain; charset=utf-8` unless a safe type is known | yes |
| `/{owner}/{repo}/blame/{ref}/{path...}` | Line-by-line blame | yes |
| `/{owner}/{repo}/commits/{ref}` | Commit list for a ref | yes |
| `/{owner}/{repo}/commits/{ref}/{path...}` | Commit list filtered to a path | yes |
| `/{owner}/{repo}/commit/{sha}` | Single commit (full SHA) | yes |
| `/{owner}/{repo}/commit/{sha}.patch`, `/commit/{sha}.diff` | Patch and diff forms | yes |
| `/{owner}/{repo}/compare/{base}...{head}` | Two-dot comparison (`{base}` is the merge base) | yes |
| `/{owner}/{repo}/compare/{base}...{head}` with a fork head `{owner}:{ref}` | Cross-fork comparison | yes |
| `/{owner}/{repo}/branches`, `/tags`, `/releases`, `/tags/{tag}` | Ref and release listing | yes |
| `/{owner}/{repo}/archive/refs/heads/{branch}.tar.gz`, `.../refs/tags/{tag}.zip`, `.../{sha}.tar.gz` | Archive downloads | yes |
| `/{owner}/{repo}/find/{ref}` | File finder (tree search at a ref) | yes |

Compatibility requirement: the archive route keeps GitHub's
`/archive/refs/heads|tags/{ref}.{tar.gz|zip}` shape, including `/{sha}.tar.gz`, so download
scripts and `curl -L` recipes keep working.

### 2.4 Issues, pull requests, and discussions

| Route | Surface |
| --- | --- |
| `/{owner}/{repo}/issues` | Issue list |
| `/{owner}/{repo}/issues/new`, `/issues/new?template=...` | Issue creation with template selection |
| `/{owner}/{repo}/issues/{number}` | Issue detail and timeline |
| `/{owner}/{repo}/issues/{number}#issuecomment-{comment-id}` | Fragment anchor for one comment |
| `/{owner}/{repo}/labels`, `/labels/{label}` | Label management and per-label issue list |
| `/{owner}/{repo}/milestones`, `/milestones/{number}` | Milestone management and per-milestone list |
| `/{owner}/{repo}/pulls` | Pull-request list |
| `/{owner}/{repo}/pulls/new`, `/compare/{base}...{head}?expand=1` | Pull-request creation |
| `/{owner}/{repo}/pull/{number}` | Pull-request conversation |
| `/{owner}/{repo}/pull/{number}/files` | Files-changed view |
| `/{owner}/{repo}/pull/{number}/commits` | Commits in the pull request |
| `/{owner}/{repo}/pull/{number}/checks` | Checks and statuses for the head SHA |
| `/{owner}/{repo}/pull/{number}/files#diff-{file-index}` | Per-file diff anchor |
| `/{owner}/{repo}/discussions`, `/discussions/new`, `/discussions/{number}` | Discussions (phase 7) |
| `/{owner}/{repo}/discussions/categories/{slug}` | Category-filtered list |

Compatibility requirement: issue and pull-request URLs use the shared per-repository number,
so `/{owner}/{repo}/issues/123` and `/{owner}/{repo}/pull/123` never refer to different
numbers. A number resolved as a pull request on the `/issues/{number}` route redirects to the
`/pull/{number}` form.

### 2.5 Projects, wiki, pages, packages, actions, security, insights

| Route | Surface |
| --- | --- |
| `/{owner}/{repo}/projects`, `/projects/{number}`, `/projects/{number}/views/{view-id}` | Repository projects (phase 7) |
| `/users/{user}/projects/{number}`, `/orgs/{org}/projects/{number}` | User and organization projects |
| `/{owner}/{repo}/wiki`, `/wiki/{page}`, `/wiki/{page}/_history`, `/_new`, `/_edit` | Wiki (phase 7) |
| `/{owner}/{repo}/pages`, `/settings/pages` | Pages status, build log, and site settings |
| `/{owner}/{repo}/pkgs/{registry}/{package}` | Package detail (phase 6) |
| `/{owner}/{repo}/releases/tag/{tag}`, `/releases/latest`, `/releases/new` | Release surfaces |
| `/{owner}/{repo}/actions`, `/actions/workflows/{file}`, `/actions/runs/{run-id}`, `/jobs/{job-id}` | Actions surfaces (phase 5) |
| `/{owner}/{repo}/settings/environments`, `/settings/environments/{name}` | Environments (phases 5, 8) |
| `/{owner}/{repo}/settings/secrets/actions`, `/settings/variables/actions` | Repository secrets and variables (masked values only) |
| `/{owner}/{repo}/settings/actions/runners` | Runner registration and labels |
| `/{owner}/{repo}/settings/branches`, `/settings/rules`, `/settings/tag_protection` | Protection and rulesets (phase 8) |
| `/{owner}/{repo}/settings/codeowners/errors` | CODEOWNERS parse errors |
| `/{owner}/{repo}/security/advisories` | Not delivered: Advanced Security is excluded; the route is reserved in the reserved-name list and never served |
| `/{owner}/{repo}/graphs/contributors`, `/graphs/commit-activity`, `/graphs/dependency-graph`, `/graphs/traffic` | Insights (phase 7) |
| `/{owner}/{repo}/network`, `/forks`, `/stargazers`, `/watchers` | Forks, stars, and watchers |

### 2.6 Repository settings and administration

| Route | Surface |
| --- | --- |
| `/{owner}/{repo}/settings` | General settings: name, description, visibility, features, archive |
| `/settings/access`, `/settings/collaborators`, `/settings/teams` | Direct and team access |
| `/settings/keys` | Deploy keys |
| `/settings/hooks`, `/settings/hooks/{hook-id}`, `/settings/hooks/{hook-id}/deliveries`, `.../deliveries/{delivery-id}` | Webhooks and delivery inspection |
| `/settings/transfer`, `/settings/delete` | Destructive operations with confirmation |
| `/settings/packages`, `/settings/pages`, `/settings/installations` | Feature-scoped settings |

## 3. API path shapes

### 3.1 Prefix and mapping

| Rule | Value |
| --- | --- |
| REST prefix | `/api/v1` |
| GraphQL endpoint | `/api/graphql` |
| Version header | `X-GitHub-Api-Version`; the path also carries the major version |
| Web-to-API mapping | Every web surface has an API resource at the same noun, addressed by `{owner}/{repo}` and a stable identifier, never by a page number |
| Owner/repo qualification | API paths always carry an explicit `{owner}` and `{repo}`. The web shorthand `/{owner}/{repo}` has no API equivalent; there is no implicit "current repository" on the API. |
| Identifier in API paths | Numeric `{number}` for issues and pull requests; `{id}` for installation, run, job, artifact, delivery, and workspace resources, where the ID is a UUID-typed identifier, not a database sequence exposed for enumeration; names and slugs for owner, repo, environment, team, package, and label where the name is the contract |
| HTML/JSON separation | The web routes return HTML; the API routes return JSON. A web route never returns a JSON body and never accepts `Accept: application/vnd.github+json` as a substitute for the API path. |

### 3.2 REST path families

| Family | Path shapes |
| --- | --- |
| Accounts and sessions | `/api/v1/login`, `/api/v1/logout`, `/api/v1/me`, `/api/v1/user`, `/api/v1/user/emails`, `/api/v1/user/keys`, `/api/v1/user/gpg_keys`, `/api/v1/user/sessions`, `/api/v1/user/tokens`, `/api/v1/user/installations` |
| Users | `/api/v1/users/{username}`, `/users/{username}/repos`, `/users/{username}/followers`, `/users/{username}/following`, `/users/{username}/starred` |
| Organizations | `/api/v1/orgs/{org}`, `/orgs/{org}/members`, `/orgs/{org}/memberships/{username}`, `/orgs/{org}/repos`, `/orgs/{org}/teams`, `/orgs/{org}/installations`, `/orgs/{org}/projects`, `/orgs/{org}/packages` |
| Teams | `/api/v1/orgs/{org}/teams/{team_slug}`, `.../members`, `.../memberships/{username}`, `.../repos`, `.../teams` (child teams) |
| Repositories | `/api/v1/repos/{owner}/{repo}`, `/api/v1/user/repos`, `/api/v1/orgs/{org}/repos` |
| Branches, tags, refs | `/repos/{owner}/{repo}/branches`, `/branches/{branch}`, `/branches/{branch}/protection`, `/tags`, `/git/refs`, `/git/refs/{ref}`, `/git/trees/{tree_sha}`, `/git/blobs/{file_sha}`, `/git/commits/{commit_sha}`, `/git/matching-refs/{ref}` |
| Contents | `/repos/{owner}/{repo}/contents/{path}`, `/readme`, `/license`, `/tarball/{ref}`, `/zipball/{ref}`, `/compare/{basehead}` |
| Commits | `/repos/{owner}/{repo}/commits`, `/commits/{ref}`, `/commits/{ref}/status`, `/commits/{ref}/check-runs`, `/commits/{ref}/check-suites` |
| Collaborators | `/repos/{owner}/{repo}/collaborators`, `/collaborators/{username}`, `/collaborators/{username}/permission`, `/invitations`, `/forks` |
| Issues | `/repos/{owner}/{repo}/issues`, `/issues/{issue_number}`, `/issues/{issue_number}/comments`, `/issues/{issue_number}/labels`, `/issues/{issue_number}/assignees`, `/issues/{issue_number}/timeline`, `/issues/comments/{comment_id}`, `/labels`, `/labels/{name}`, `/milestones`, `/milestones/{milestone_number}` |
| Pull requests | `/repos/{owner}/{repo}/pulls`, `/pulls/{pull_number}`, `/pulls/{pull_number}/files`, `/commits`, `/comments`, `/reviews`, `/reviews/{review_id}`, `/reviews/{review_id}/comments`, `/comments/{comment_id}`, `/merge`, `/mergeability`, `/auto_merge`, `/review_requests`, `/requested_reviewers` |
| Releases | `/repos/{owner}/{repo}/releases`, `/releases/{release_id}`, `/releases/tags/{tag}`, `/releases/latest`, `/releases/{release_id}/assets`, `/releases/assets/{asset_id}` |
| Packages | `/repos/{owner}/{repo}/packages`, `/orgs/{org}/packages`, `/users/{username}/packages`, `/packages/{package_type}/{package_name}`, `.../versions`, `.../restore` |
| Registries (protocol, not REST) | `/v2/` (OCI), `/api/packages/npm/...`, `/api/packages/maven/...`, `/api/packages/nuget/...`, `/api/packages/rubygems/...`, `/api/packages/composer/...`, `/api/packages/helm/...`, `/api/packages/generic/{name}/{version}/{file}` |
| Actions and runners | `/repos/{owner}/{repo}/actions/workflows`, `.../workflows/{workflow_id}`, `.../workflows/{workflow_id}/dispatches`, `.../runs`, `.../runs/{run_id}`, `.../runs/{run_id}/cancel`, `.../rerun`, `.../logs`, `.../jobs`, `.../jobs/{job_id}`, `.../jobs/{job_id}/logs`, `.../artifacts`, `.../artifacts/{artifact_id}`, `.../cache`, `.../secrets`, `.../variables`, `.../environments`, `.../environments/{environment_name}`, `.../environments/{environment_name}/deployments`; runner protocol `/api/v1/runner/*` |
| Webhooks and apps | `/repos/{owner}/{repo}/hooks`, `/hooks/{hook_id}`, `/hooks/{hook_id}/deliveries`, `.../deliveries/{delivery_id}`, `/orgs/{org}/hooks`, `/app`, `/app/installations`, `/app/installations/{installation_id}`, `/installation/repositories`, `/app/installations/{installation_id}/access_tokens` |
| OAuth | `/login/oauth/authorize`, `/login/oauth/access_token` (web-surface paths, not under `/api/v1`) |
| Search and discovery | `/api/v1/search/repositories`, `/search/code`, `/search/issues`, `/search/commits`, `/search/users`, `/search/topics`, `/search?q=&type=` |
| Notifications | `/api/v1/notifications`, `/notifications/{thread_id}`, `/notifications/{thread_id}/subscription`, `/repos/{owner}/{repo}/subscription`, `/notifications/threads/{thread_id}/subscription` |
| Activity and audit | `/api/v1/users/{username}/events`, `/repos/{owner}/{repo}/events`, `/orgs/{org}/events`, `/orgs/{org}/audit-log`, `/enterprises/{enterprise}/audit-log` |
| Governance | `/repos/{owner}/{repo}/rulesets`, `/rulesets/{ruleset_id}`, `/orgs/{org}/rulesets`, `/repos/{owner}/{repo}/keys` (deploy keys), `/repos/{owner}/{repo}/environments`, `/deployments`, `/deployments/{deployment_id}/statuses` |
| Projects and discussions | `/repos/{owner}/{repo}/projects`, `/orgs/{org}/projects`, `/users/{username}/projects`, `/projects/{project_id}` (columns/fields/items), `/repos/{owner}/{repo}/discussions`, `/discussions/{discussion_number}`, `/discussions/{discussion_number}/comments`, `/discussions/{discussion_number}/comments/{comment_id}/replies`, `/discussions/categories` |
| Wiki and pages | `/repos/{owner}/{repo}/wiki` (page list, get, put, delete), `/repos/{owner}/{repo}/pages`, `/pages/builds`, `/pages/deployments`, `/repos/{owner}/{repo}/pages/domains` |
| Workspaces | `/api/v1/repos/{owner}/{repo}/workspaces`, `/api/v1/workspaces`, `/api/v1/workspaces/{workspace_id}`, `.../start`, `.../stop`, `.../rebuild`, `.../ports`, `.../snapshots`, `.../shares`, `.../secrets`, `.../variables` |
| Rate limit and metadata | `/api/v1/rate_limit`, `/api/v1/meta`, `/api/health`, `/api/ready` |

Compatibility requirement: for a resource GitNest implements, the path shape, JSON key names
(`snake_case`), and pagination parameters match GitHub's REST API so an existing client
library works after only a base-URL change.

## 4. Git remote URLs

### 4.1 Generation

| Form | Shape | When offered |
| --- | --- | --- |
| HTTPS | `https://<host>/<owner>/<repo>.git` | Always, on the repository clone box and the empty-repository page |
| HTTPS with token | `https://<token>@<host>/<owner>/<repo>.git` | Never displayed; shown only as a copy button that injects the caller's own fresh token client-side, never in page source, logs, or referrers |
| SSH (scp-like) | `git@<ssh-host>:<owner>/<repo>.git` | When the SSH entry point is configured |
| SSH (URL) | `ssh://git@<ssh-host>:<port>/<owner>/<repo>.git` | When a non-default SSH port is configured |
| Wiki (HTTPS) | `https://<host>/<owner>/<repo>.wiki.git` | On the wiki surface (phase 7) |
| Pages | `https://<owner>.<pages-host>/<repo>/`, `https://<pages-host>/<owner>/<repo>/`, or a verified custom domain | Per Pages configuration (phase 7) |
| Preview (workspace) | `https://{port}-{workspace-id}.<preview-host>/` | Phase 9 |

Rules:

- `<host>` is the host and optional port of `GITNEST_PUBLIC_URL`, including any configured
  path prefix; `<ssh-host>` and `<ssh-port>` are separate instance settings because the SSH
  endpoint may bind a different host or port than the web origin.
- The repository segment is always `<owner>/<repo>.git` with the owner's current name and
  slug-CI casing, never a UUID.
- A renamed owner or repository changes the remote URL. The old URL keeps working as a Git
  redirect: clone, fetch, and push against the old path receive `301` to the new path, and an
  SSH path resolves the old name through the same redirect table.
- The `.git` suffix is accepted and optional on requests; a request without it is served
  directly rather than redirected, so both forms of an existing remote work.
- A case-different path resolves when it case-folds to exactly one repository; it redirects to
  the canonical casing.
- Anonymous fetch of a public repository works with no credential. Anything else requires HTTP
  Basic with a personal access token, OAuth/app token, or an SSH key.

### 4.2 Git smart HTTP and protocol endpoints

| Route | Purpose |
| --- | --- |
| `/{owner}/{repo}.git/info/refs?service=git-upload-pack` | Ref advertisement for clone and fetch |
| `/{owner}/{repo}.git/info/refs?service=git-receive-pack` | Ref advertisement for push |
| `/{owner}/{repo}.git/git-upload-pack` | Fetch pack negotiation (protocol v2 supported) |
| `/{owner}/{repo}.git/git-receive-pack` | Push |
| `/{owner}/{repo}.git/info/lfs/objects/batch`, `.../lfs/objects/{oid}` | Git LFS (phase 1) |

These routes are registered for `POST` and `GET` only as the protocol requires; every other
method returns `405`. Authorization happens before any Git process starts, through the shared
repository service.

## 5. Redirects

| Situation | Behaviour | Honoured for |
| --- | --- | --- |
| Repository renamed within the same owner | Old web and API paths `301` to the new path; old Git paths `301` too | Until the old name is taken by another repository of that owner, or the redirect record is 12 months old and pruned by a job; the record is never dropped while it is the only path a live remote uses |
| Repository transferred to another owner | Old `{old-owner}/{repo}` paths `301` to `{new-owner}/{repo}`; Git remotes follow the same redirect | Same rule as rename |
| User or organization renamed | All `/{owner}/...` paths and the profile `301` to the new owner segment | Same rule |
| Issue or pull request transferred to another repository | Old `/{owner}/{repo}/issues/{number}` resolves to the new location with a moved notice | Record retained with the number redirect; the old number is never reused in the source repository |
| Wiki page renamed | Documented rule of either a redirect page or inbound-link update, chosen and tested in phase 7 | Same rule |
| Default branch renamed | `/{owner}/{repo}/tree/{old-default}` continues to resolve while the old ref exists as a Git ref; no redirect record is created for a ref | While the ref exists |
| Repository deleted | No redirect; `404` for every path. A redirect record for a deleted repository is removed, not reused for a future repository of the same name | — |
| Visibility changed public to private | No redirect; an unauthorized caller now receives `404`, and cacheability drops to private (section 9) | — |

Compatibility requirement: a redirect chain is collapsed to one hop. GitNest never issues a
redirect to a path that itself redirects.

## 6. Identifier rules

### 6.1 Common rules

| Rule | Value |
| --- | --- |
| Comparison | Owner names, team slugs, repository names, label names, milestone titles, environment names, and package names are compared after Unicode-free ASCII case folding. Display preserves the case chosen at creation. |
| Stored form | `name` (display, as created) and `name_folded` (unique index) are separate columns; the unique constraint is on the folded form. |
| Allowed characters | ASCII letters, digits, and the separators listed per identifier. No control characters, no leading or trailing separator, no consecutive separators where stated, no Unicode normalization ambiguity. |
| Rejected inputs | Empty string, `.` or `..`, anything containing `/`, `\`, NUL, a control character, a leading `-` (except where stated), trailing `.` or `.git`, or whitespace other than where stated. Rejection is a validation error, not a silent rewrite. |
| Length | Counted in Unicode code points for display names and in ASCII bytes for slugs; the limit is on the stored folded form. |
| Uniqueness scope | Stated per identifier. A scope is a repository, an organization, or the whole instance. |
| UUID | Every entity additionally has a stable UUID used in storage, foreign keys, and events. It never appears in a URL and never appears in a user-visible payload. |

### 6.2 Username, organization, and enterprise names

| Property | Rule |
| --- | --- |
| Grammar | `^[A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9]))*$` — alphanumerics and single hyphens; no leading or trailing hyphen, no consecutive hyphens |
| Length | 1–39 characters (GitHub compatibility: maximum 39) |
| Uniqueness scope | Instance-wide, shared across usernames, organization logins, and enterprise slugs, so `/{owner}` is unambiguous |
| Reserved | A leading `-` is rejected; names in the reserved list (6.9) are rejected case-insensitively; an existing account cannot take a name that is a live redirect source for another account |
| Display name | Free text, up to 255 characters, not unique, may change without changing the URL |
| Organization slug vs name | The URL segment is the organization `login`. The display name is free text. Changing the login creates a redirect (section 5). |
| Enterprise slug | Same grammar, length 1–39, reserved list applies, unique instance-wide and disjoint from usernames and organizations |
| Machine users | Same grammar and uniqueness as a user; kind is a stored attribute, not a naming convention, so renaming cannot grant interactive login |

### 6.3 Repository names

| Property | Rule |
| --- | --- |
| Grammar | `^[A-Za-z0-9._-]+$` with: at least one alphanumeric; no leading `-`, `.`, or `_`; no trailing `-`, `.`, or `_`; no `..`; the folded form must not be `.` or `..`; the folded form must not end in `.git` |
| Length | 1–100 characters (GitHub compatibility: maximum 100) |
| Uniqueness scope | Per owner: a user or organization cannot have two repositories whose folded names are equal. Two different owners may each hold `api`. |
| Reserved | Names in the reserved list (6.9) cannot be created at the top level of an owner; a repository may be named `settings` only if it does not collide with a route, and the collision check is authoritative |
| Display | The case chosen at creation is displayed; URLs are canonicalized to it and case-different paths `301` |
| Rename | Allowed if the target name is free for that owner; the old name becomes a redirect (section 5); the storage path does not change because it derives from the UUID |
| Default branch | A branch name, not an identifier constraint of the repository; validated by Git ref rules: no `..`, no leading or trailing `/`, no control characters, no `@{`, no `\`, and it is a valid refname |
| Archive state | Archiving changes mutability, not the name or the URL |

### 6.4 Team names and slugs

| Property | Rule |
| --- | --- |
| Name | Display name, up to 255 characters, free text |
| Slug | Derived from the name: lowercase, non-alphanumeric runs replaced by `-`, trimmed of leading and trailing `-`; must be 1–100 characters and non-empty after derivation |
| Uniqueness scope | Per organization, on the derived slug. Two teams in one organization cannot share a slug; the same slug may exist in another organization. |
| Nested teams | The URL uses `/orgs/{org}/teams/{team_slug}` and child teams are listed, not addressed by path, matching GitHub; a team cannot be its own ancestor (cycle guard) |
| Renaming | Changing the name changes the slug and creates a slug redirect honoured under the section 5 retention rule |

### 6.5 Label and milestone names

| Property | Rule |
| --- | --- |
| Label name | 1–50 characters, any printable characters except a leading or trailing space; case-folded uniqueness per repository |
| Label color | Six hexadecimal digits, no `#` in storage, rendered with `#` |
| Milestone title | 1–255 characters; case-folded uniqueness per repository across open and closed milestones |
| Milestone route | `/{owner}/{repo}/milestone/{number}`, numbered per repository in its own sequence, never reused |
| Reserved | Neither labels nor milestones have reserved names; they carry no route segment of their own beyond the numeric milestone route |

### 6.6 Package names and versions

| Property | Rule |
| --- | --- |
| npm scope | `@scope/name` maps to the owner namespace; the scope must be a valid owner name |
| npm name | Lowercase, up to 214 characters, matching the npm grammar; publish validates the name against the registry grammar and never concatenates it into a filesystem path |
| OCI repository | Lowercase path components, `[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*` segments separated by `/`, up to 255 characters |
| OCI tag | `[A-Za-z0-9_][A-Za-z0-9._-]{0,127}`; a digest reference is `sha256:` plus 64 hex characters and is the immutable form |
| Maven | `groupId:artifactId:version` as path segments; a release version is immutable once published |
| NuGet | ID 1–100 characters, case-insensitive for lookup and case-preserving for display |
| RubyGems | Lowercase name matching the RubyGems grammar; yank removes the version from the index while keeping an exact-URL download |
| Generic | `<name>/<version>/<file>` with each segment validated separately against a conservative grammar; the storage key derives from internal IDs and the content digest |
| Uniqueness scope | A package is unique on `(registry kind, owner, folded name)`; a version is unique within a package |
| Visibility | Inherited from the owning repository or organization. There is no separate package visibility switch. |

### 6.7 Environment names

| Property | Rule |
| --- | --- |
| Grammar | Up to 255 characters; must start with a letter or `_`; may contain letters, digits, `-`, `_`, `.`, and spaces; no control characters |
| Uniqueness scope | Per repository, case-folded |
| Addressing | By name in API paths (`/environments/{environment_name}`) and in the environment settings URL; names appear in URLs, so a name that is a URL-reserved path segment is percent-encoded rather than rejected |
| Protection | Required reviewers (bounded maximum), wait timer, branch and tag restrictions, required checks, and environment-scoped secrets and variables (phases 5 and 8) |

### 6.8 Workspace names and identifiers

| Property | Rule |
| --- | --- |
| Identity | A workspace is identified by a UUID. Routes use the UUID: `/workspaces/{workspace-id}`, `/workspaces/{workspace-id}/terminal`. Storage paths, volume names, and container names derive from the UUID. |
| Display name | An optional non-unique label, up to 100 characters, for the workspace list only; it never appears in a URL or a storage path |
| Scratch workspaces | A scratch workspace (the Gist replacement) has no repository and no required name; exporting it to a repository creates a repository name validated by section 6.3 |
| Preview host | `{port}-{workspace-id}.<preview-host>`; the workspace is resolved from a signed route or a stored host mapping, never from a client-supplied address |

### 6.9 Reserved names

Reserved names are rejected for usernames, organization logins, enterprise slugs, and the
first segment of an owner-scoped route. The list is enforced case-insensitively and is
versioned with the code; adding a name never renames an existing account, it only blocks new
registrations.

| Category | Names |
| --- | --- |
| Infrastructure | `api`, `apis`, `app`, `apps`, `assets`, `auth`, `camo`, `cdn`, `collector`, `graphql`, `health`, `hooks`, `internal`, `live`, `metrics`, `media`, `objects`, `proxy`, `raw`, `ready`, `static`, `status`, `storage`, `uploads`, `v2` |
| Product surfaces | `about`, `account`, `admin`, `billing`, `blog`, `codespaces`, `contact`, `dashboard`, `discussions`, `enterprise`, `enterprises`, `explore`, `features`, `gist`, `gists`, `home`, `issues`, `jobs`, `join`, `login`, `logout`, `marketplace`, `new`, `notifications`, `organizations`, `orgs`, `packages`, `pages`, `pricing`, `projects`, `pulls`, `register`, `search`, `security`, `sessions`, `settings`, `signup`, `site`, `sponsors`, `stars`, `teams`, `terms`, `topics`, `trending`, `users`, `watching`, `wiki`, `workspaces` |
| Git protocol | `git-upload-pack`, `git-receive-pack`, `info`, `lfs`, `objects` |
| Dot-prefixed and traversal | `.`, `..`, `.git`, `.github`, `.well-known`, any name starting with `.` or `-` |
| GitNest-specific | `gitnest`, `_health`, `_status` |

`codespaces`, `gist`, `gists`, `sponsors`, `marketplace`, and `security` remain reserved even
though the corresponding products are excluded, because a future route or a migration import
could otherwise collide with a user-chosen name.

## 7. Issue and pull-request numbering

| Rule | Requirement |
| --- | --- |
| Sequence | One sequence per repository shared by issues and pull requests, allocated in the same transaction that inserts the row, under a repository row lock or an atomic `UPDATE ... RETURNING` |
| Start | A repository's first issue or pull request is `1` |
| Contiguity | Numbers are contiguous with no gaps from failed transactions; a rolled-back insert does not consume a number |
| Concurrency | Two concurrent creates never share a number, proven by a concurrency test |
| Never reused | A number is never reused after deletion, transfer, or reopen. Numbers are not recycled when an issue is deleted. |
| Transfer | Transferring an issue or pull request to another repository allocates a new number in the target and keeps a redirect from the old location; the old number is retired in the source repository |
| Pull requests | A pull request consumes the shared number at creation, so `#123` is unambiguous across both |
| Milestones | A separate per-repository sequence (`/milestone/{number}`) with the same contiguity and no-reuse rules |
| Display | `#123` in rendered Markdown resolves within the current repository only; `owner/repo#123` requires read access to the target at render time and renders as plain text when access is absent |
| API stability | The number is the API identifier for issues and pull requests and is immutable for the life of the resource |

## 8. Content- and commit-addressed routes

| Addressing form | Example | Resolution | Canonical? |
| --- | --- | --- | --- |
| Branch name | `/tree/main/src` | Looked up as `refs/heads/main` first | Canonical form for the default branch |
| Tag name | `/tree/v1.2.0` | Looked up as `refs/heads/*`, then `refs/tags/*` | Canonical for a released tag |
| Full 40-hex SHA | `/tree/8f3c...e21/src` | Resolved to the commit object; never looked up as a branch, even if a branch has that name (the ref wins for the ref route, the SHA route is explicit) | Canonical for an immutable target |
| Full SHA on a commit route | `/commit/8f3c...e21` | Immutable | Canonical, and the only immutable commit route |
| Abbreviated SHA | `/commit/8f3c1a2` | Resolved to a unique object; ambiguous prefixes are rejected with a specific error, never guessed | Not canonical |
| `HEAD` | `/tree/HEAD` | Resolved to the default branch | Not canonical |
| Default-branch shortcut | `/{owner}/{repo}` (overview, no ref) | Equivalent to `/{owner}/{repo}/tree/{default-branch}` for the code surface | Canonical for the repository landing page |
| Blob digest (Git object) | API `/git/blobs/{file_sha}` | Content-addressed by the Git object ID | Canonical for the API object route |
| Package blob digest | `/v2/<name>/blobs/sha256:...` | Content-addressed and immutable | Canonical |
| Release asset | `/releases/download/{tag}/{asset-name}` | Resolved to an asset row on the release; the download is content-typed and range-capable | Canonical, with the asset ID form as the API alias |

Rules:

1. **Refs win over SHA interpretation.** A route that accepts a ref resolves it as a Git ref
   first; a 40-hex string that is also a branch name resolves to the branch on ref routes and
   to the commit on its own commit route. The two routes are distinct so the behaviour is
   never ambiguous.
2. **Immutable routes are the citation form.** A URL that must not change meaning over time
   (a check-run details link, a review-comment anchor, a release asset link, a webhook payload
   URL) uses the full SHA or a digest, never a branch name.
3. **Branch and tag routes are the human form.** They are what the UI links to by default,
   because they survive forward movement of the branch in a way a SHA does not.
4. **Ref names are validated in the service**, not in the transport, against Git refname rules
   and the reserved `refs/` namespaces. A ref that does not exist returns the same not-found
   response as an inaccessible repository.
5. **`/compare/{base}...{head}`** accepts ref names, SHAs, and fork-qualified heads
   (`{owner}:{ref}`). A two-dot comparison (`..`) is not a distinct route.

## 9. Pagination and sorting

| Parameter | Applies to | Rule |
| --- | --- | --- |
| `page` | Offset-paginated collections | 1-based; default 1; a non-integer or `< 1` is a validation error; a page beyond the last returns an empty array with correct link relations |
| `per_page` | Offset-paginated collections | Default 30, maximum 100. A value above the maximum is clamped to 100, not rejected (GitHub compatibility) |
| `cursor` | Cursor-paginated collections (feeds, search results, large lists) | Opaque; treated as an untrusted input, bound to the filter set that produced it; an expired or mismatched cursor is a validation error |
| `limit` | Cursor-paginated collections | Optional page size with the same 100 maximum; `per_page` and `limit` are not both honoured on one endpoint |
| `sort` | Per-endpoint allowlist | An unknown value is a validation error naming the parameter |
| `direction` | Where `sort` exists | `asc` or `desc`; default per endpoint, documented in the endpoint reference |
| `state` | Issues, pull requests, projects | `open`, `closed`, `all` where the resource has those states |
| `since`, `until` | Timestamp-filtered lists | RFC 3339 UTC, inclusive on both ends |
| `q` | Search | The qualifier grammar owned by `internal/search`; an unknown qualifier is a validation error naming it |

Rules:

| Rule | Requirement |
| --- | --- |
| Filter before count | Filters, visibility checks, and permission checks are applied before pagination and before any total or facet count. A private resource never shifts a public total. |
| Link header | Any paginated collection with more than one page carries `Link` with `rel="next"`, `"prev"`, `"first"`, and `"last"` as applicable; relations are omitted, never emitted empty |
| Stable order | Every list has a deterministic tiebreak (normally the internal UUID for cursor lists and a stable sort key plus ID for offset lists) so a page boundary cannot duplicate or skip a row |
| Default sort | Documented per endpoint. Issues and pull requests default to newest-first by creation; repository lists default to push recency; commits default to commit-date descending in Git order |
| Rate and cost | Search, diff, and archive endpoints carry separate rate limits and never recompute a total on a large corpus in one request |
| Cursor stability | A cursor is valid across a bounded window; a visibility change or a deleted row may drop an item, but never resurrect an item the caller may no longer read |

## 10. Route, authorization, and cacheability

Authorization is decided by the shared application service, never by the transport. The
"Requirement" column names the decision the service returns. Cacheability is the HTTP response
policy for the transport, expressed as `Cache-Control` and `Vary`.

| Route family | Requirement | Cacheability |
| --- | --- | --- |
| `/`, `/explore`, `/topics`, `/search` (anonymous, public content only) | None (public) | `public, max-age=60`, `Vary: Accept, Accept-Encoding` |
| `/login`, `/signup`, `/password_reset` | None | `no-store` |
| `/settings/**`, `/notifications`, `/workspaces/**`, `/enterprises/**` | Authenticated; workspace routes additionally require the workspace access decision | `private, no-store`, `Vary: Cookie` |
| `/{owner}` profile and `/orgs/{org}/**` | None for a public owner; read membership for a private-visible surface | `public, max-age=60` when the payload is fully public, otherwise `private, max-age=0` |
| `/{owner}/{repo}` overview, `/{owner}/{repo}/tree/**`, `/blob/**`, `/raw/**`, `/blame/**`, `/commits/**`, `/compare/**`, `/tags`, `/branches`, `/network`, `/forks` | Repository read, or anonymous when public | Public repository and anonymous caller: `public, max-age=300` for a SHA-addressed tree, blob, or commit; `public, max-age=60` for a ref-addressed one; `Vary: Accept-Encoding`. Private or authenticated: `private, max-age=0, must-revalidate` |
| `/{owner}/{repo}/issues`, `/pulls`, `/discussions`, `/projects`, `/releases`, `/wiki`, `/graphs/**` | Repository read; anonymous only for public repositories | Public: `public, max-age=60`; private: `private, no-store`. Never cached across a visibility change (invalidation on the permission event) |
| `/{owner}/{repo}/issues/{number}`, `/pull/{number}` and sub-views | Repository read; a private resource is `404` to an unauthorized caller | Public: `public, max-age=30`; private: `private, no-store` |
| `/{owner}/{repo}/actions/**` run, job, and log views | Repository read; runner-token paths are not browser-reachable | `private, no-store` (logs are secret-masked and must never be shared between viewers) |
| `/{owner}/{repo}/settings/**`, `/settings/**`, `/orgs/{org}/settings/**` | Repository admin or organization owner | `private, no-store` |
| `/api/v1/**` | Per endpoint: public, authenticated, or a specific permission | JSON responses carry `Cache-Control: private, must-revalidate` with a strong `ETag`; a public read-only resource may carry `public, max-age=60`. `Vary: Accept, Accept-Encoding, Authorization, X-GitHub-Api-Version` |
| `/api/health`, `/api/ready` | None, but bound to an operator-only listener where configured | `no-store` |
| `/{owner}/{repo}.git/info/refs`, `git-upload-pack` | Repository read; anonymous only for a public repository | `no-store` by protocol requirement |
| `/{owner}/{repo}.git/git-receive-pack` | Repository write plus branch protection and ruleset checks | `no-store` |
| `/{owner}/{repo}.git/info/lfs/**` | Repository read for download, write for upload | `no-store` |
| `/{owner}/{repo}/releases/download/**`, `/archive/**` | Repository read | `public, max-age=300, immutable` when addressed by tag, SHA, or digest on a public repository; `private, no-store` otherwise |
| `/v2/**` and `/api/packages/**` | Package read or write through the permission registry | Manifest and blob responses by digest: `public, max-age=31536000, immutable` on a public package, `private, no-store` otherwise; upload paths `no-store` |
| `/login/oauth/authorize`, `/login/oauth/access_token`, `/app/**`, `.../access_tokens` | None for the flow entry point; app-authenticated for installations | `no-store` |
| `/{owner}/{repo}/pages`, `/pages/**` | Repository admin for management; published content follows the Pages visibility rule | Management: `private, no-store`. Published artifact: `public, max-age=300` for a public site, `private, no-store` for a private repository's site |
| `https://{port}-{workspace-id}.<preview-host>/**` | Workspace preview access decision; an unauthorized caller receives `404` so port existence is not disclosed | `private, no-store` by default; an explicitly public preview is out of scope unless an operator enables it |

Cacheability rules:

- A response whose authorization depends on the caller is never stored in a shared cache. The
  transport marks it `private` or `no-store`; no route relies on `Vary: Cookie` alone.
- A cached entry is keyed by a value that already accounts for the viewer's permission, so two
  callers never share a rendered payload one of them may not read.
- Revoking access, changing visibility, or renaming invalidates the affected cache entries by
  an explicit signal, not by expiry (phase 10 owns the shared cache; phases 1–9 correct with
  an in-process cache and short `max-age` values).
- ETags are derived from the resource version plus a content digest, and a `304` never carries
  a regenerated body.

## 11. Open questions this document does not answer

These need a product decision before the owning phase lands, and are recorded rather than
guessed:

1. Whether wiki page renames keep a redirect page or rewrite inbound links (phase 7 chooses
   and tests one).
2. The exact retention window for rename and transfer redirect records beyond the 12-month
   default above, and whether an operator can extend it.
3. Whether the API exposes a numeric `id` alongside the number for issues and pull requests
   for GitHub payload compatibility, or only the number.
4. Whether per-endpoint `sort` defaults are published per endpoint or derived from one table.
5. Whether the reserved-name list is operator-extensible at runtime or fixed per release.
