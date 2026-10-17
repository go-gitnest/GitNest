# Discovery and content surfaces roadmap

**Phase:** 7
**Status:** not started
**Roadmap file:** docs/roadmap/07-discovery-and-surfaces.md

## Purpose

Phase 7 makes an existing instance navigable and adds the content surfaces that sit around
repositories: search, topics, social signals, a Git-backed wiki, static site hosting,
Discussions, Projects, insights, and profile pages. The recurring constraint is that the
single-node baseline must stay sufficient: search and static hosting introduce heavier
components only when a workstream genuinely needs them, and every such component has a
defined degraded mode. All discovery surfaces must respect the visibility rules already
enforced by the repository permission service.

## Scope

Search across repositories, code, issues and pull requests, commits, users, and topics, with
qualifiers and a pluggable index that degrades to SQL on small instances. Topics, stars,
watching, follows, and an explore/trending surface. Per-repository Git-backed wiki. GitHub
Pages-style static hosting with a build step, custom domains, and HTTPS. Discussions with
categories, answers, polls, and Q&A. Projects with boards, tables, and roadmaps, custom
fields, views, and automations. Repository insights: contributors, commit activity, traffic,
and dependency graph. User and organization profile pages.

## Workstreams

### W7.1 Search interface and SQL fallback

- **Deliverable:** A search port with a query model (terms, qualifiers, sort, cursor
  pagination, scope) and a PostgreSQL implementation built on `tsvector` columns and
  generated indexes. The index implementation is selectable by configuration; the SQL path
  is the default and requires no extra service.
- **Depends on:** None
- **Acceptance:** All discovery surfaces consume the port rather than issuing their own
  queries. The SQL implementation passes the shared conformance test suite used by any future
  index backend. A configured-but-unreachable external index falls back to SQL, logs the
  degradation, and serves results rather than failing the request. The same query model
  produces identical results through the SQL path for the conformance fixture set.

### W7.2 Repository and commit search

- **Deliverable:** Repository search over name, description, and topics with visibility
  filtering applied before ranking and pagination, plus commit search over commit message,
  author, and date range within repositories the caller can read.
- **Depends on:** W7.1
- **Acceptance:** A private repository never appears in results for a caller without read
  access, including through result counts or facet totals; the denied case is tested against
  both the SQL and index paths. Pagination is stable while new repositories are created.
  Commit search never returns commits from unreadable repositories even when the same commit
  object exists in a readable fork.

### W7.3 Code search

- **Deliverable:** Code search over the default branch (and optionally an explicitly selected
  branch) using indexed path and content terms, with language detection, path filtering, and
  result snippets that show the matching line with surrounding context.
- **Depends on:** W7.1
- **Acceptance:** Searching a private repository without access returns no results and no
  counts. Snippets escape file content and never emit repository HTML. Indexing excludes
  files above the configured size cap and binary files, and the exclusion rule is documented
  and tested. A repository that has never been indexed is either excluded or searched through
  the SQL fallback, never reported as empty.

### W7.4 Issue, pull-request, and user search with qualifiers

- **Deliverable:** A qualifier parser shared by all search surfaces (`repo:`, `org:`, `user:`,
  `is:`, `state:`, `label:`, `milestone:`, `author:`, `assignee:`, `reviewer:`, `language:`,
  `topic:`, `created:`, `updated:`, `merged:`, `stars:`, `path:`, `extension:`) that rejects
  unknown qualifiers with a specific error, plus search over issues and pull requests and
  over users and organizations by name and profile text.
- **Depends on:** W7.1
- **Acceptance:** Unknown or malformed qualifiers return a validation error naming the
  qualifier rather than silently ignoring it. Qualifier filters and permission filtering
  compose so a `repo:` qualifier for an unreadable repository yields no results, proven by a
  test. Date and numeric qualifiers are parsed with an explicit grammar and covered by
  boundary tests.

### W7.5 Search indexing pipeline and degradation

- **Deliverable:** Index writes driven by durable `internal/jobs` records enqueued from
  domain events (repository update, push, issue change), with per-repository backfill,
  idempotent reindex, and a documented degraded behavior: the SQL path searches directly, and
  an external index, when configured, may lag with an explicit "results may be incomplete"
  signal bounded by measured backfill lag.
- **Depends on:** W7.1, W7.2, W7.3, W7.4
- **Acceptance:** Stopping the worker and restarting it leaves no unindexed repository after
  backfill completes, asserted by a test that inspects the index for every fixture
  repository. Reindexing the same repository twice produces identical results. Deleting or
  privatizing a repository removes it from the index within the documented window, and the
  window is tested by a visibility change followed by a search.

### W7.6 Topics

- **Deliverable:** Topic records with a normalized slug, display names, and descriptions,
  repository topic management by users with admin permission, topic pages listing
  repositories, and topic visibility filtering. A small curated set of featured topics is
  operator-configurable, not user-editable.
- **Depends on:** W7.1
- **Acceptance:** Topic slugs are unique after case folding and never collide with reserved
  slugs. A repository cannot hold duplicate topics. A topic page shows only repositories the
  caller can read, and a topic that exists solely on private repositories is not listed on a
  public topic index.

### W7.7 Stars and watching

- **Deliverable:** Star and unstar use cases with per-repository counts, a starred-repository
  list per user, watcher state per repository (participating, all activity, ignore), and a
  watch toggle surfaced on the repository page and in the API.
- **Depends on:** W7.1
- **Acceptance:** Starring is idempotent under concurrent requests, enforced by a unique
  constraint, and the count is derived consistently from the same source used by search
  ranking. Starring does not grant or imply read access to a private repository, which is
  tested. Watch state changes affect notification fan-out from Phase 2 on the next event.

### W7.8 Follows and activity feeds

- **Deliverable:** Follow and unfollow for users, a followers/following list with visibility
  filtering, and a personal activity feed assembled from public activity events of followed
  users and starred or watched repositories, with cursor pagination.
- **Depends on:** W7.7, Phase 2 W2.15
- **Acceptance:** A follow never exposes activity from a repository the viewer cannot read,
  asserted with a fixture where the followed user works in a private repository. The feed is
  stable under pagination while new events arrive. Blocking or disabling an account removes
  it from feeds and follow lists on the next request.

### W7.9 Explore and trending

- **Deliverable:** An explore page with featured topics, recently active public repositories,
  and trending repositories computed from a rolling window of stars, forks, and pushes, plus
  topic-specific trending. Ranking inputs and the window are documented and operator-tunable.
- **Depends on:** W7.6, W7.7
- **Acceptance:** Trending uses only public repositories and never leaks private repository
  names or counts. Ranking is deterministic for a fixed window and input set, so a recompute
  produces the same order and is covered by a test. An instance with no traffic renders an
  empty state rather than an error.

### W7.10 Wiki

- **Deliverable:** Per-repository wiki stored as a separate bare Git repository beside the
  code repository, with in-browser page create, edit, rename, and delete committed through
  the same bounded Git command path, a sidebar, page history, and Markdown rendering through
  the Phase 2 renderer.
- **Depends on:** Phase 2 W2.1, Phase 1 repository storage
- **Acceptance:** Wiki edits require repository write permission and reads require read
  permission, tested in both directions. Renaming a page keeps a redirect or updates inbound
  links per the documented rule, and the choice is tested. Concurrent edits to the same page
  fail with a conflict instead of losing a revision. A wiki page is not reachable when the
  code repository is private and the caller has no access.

### W7.11 Static site hosting with build step

- **Deliverable:** `internal/pages` publishing pipeline: a build job that materializes a
  configured source directory (or runs an operator-configured build command in the runner
  boundary introduced in Phase 5) into a published artifact, storage through
  `internal/platform/storage` with a local filesystem default, immutable versioned artifacts,
  an atomic pointer switch on publish, and same-origin serving under a documented path.
- **Depends on:** Phase 1 storage, Phase 5 runner for build execution
- **Acceptance:** A publish either fully replaces the served artifact or leaves the previous
  one serving; a failed build never serves a partial site. Published content is served with
  correct content types and without a directory listing. Publishing requires repository write
  permission and the denied case is tested. Sites for private repositories are served only to
  authenticated callers with access.

### W7.12 Custom domains and HTTPS for Pages

- **Deliverable:** Custom domain registration per site with a DNS verification step (TXT
  record or file), a certificate lifecycle for the configured mode (reverse proxy or
  operator-provided certificates), and documented single-node operator steps for DNS, TLS,
  and renewal, including the degraded mode where the operator terminates TLS at the proxy and
  GitNest only serves HTTP behind it.
- **Depends on:** W7.11
- **Acceptance:** An unverified or expired domain stops serving rather than serving content
  to an unverified hostname; verification state is visible to the site owner. A domain
  claimed by one site cannot be claimed by another, and the conflict is tested. Removing a
  domain stops routing to the site. The documented proxy-only mode works with no certificate
  management inside GitNest.

### W7.13 Discussions

- **Deliverable:** Discussion categories per repository with a format (open discussion,
  question and answer, announcement), discussion create/edit/close, threaded replies,
  upvotes, marking an answer for Q&A and announcements, polls with options and single or
  multiple choice voting, and an unanswered-question filter.
- **Depends on:** Phase 2 W2.1, W2.6, W2.13
- **Acceptance:** Only one answer can be marked per Q&A discussion, and re-marking moves the
  answer and is audited. Poll votes are unique per user per poll and cannot be changed after
  a documented cutoff; concurrent votes cannot exceed the choice limit. Category permissions
  are enforced server-side and tested for allowed and denied cases. Discussions honor the
  same visibility rules as issues for private repositories.

### W7.14 Projects

- **Deliverable:** `internal/projects` with projects scoped to a repository, user, or
  organization; boards, tables, and roadmaps as views over the same items; custom fields with
  documented types (text, number, date, single select, iteration); grouping, sorting, and
  filtering per view; item linking to issues and pull requests; and bounded automations
  (item added, field changed, item closed) with an audit record per run.
- **Depends on:** Phase 2 W2.2, Phase 3 W3.5
- **Acceptance:** A project item referencing an issue reflects issue state and never
  duplicates the issue record. A view's filter and sort are reproducible from the stored view
  definition, asserted by a test that rebuilds the view and compares item order. Custom
  field values are validated by field type on write, including rejected out-of-range dates
  and unknown select options. Automations cannot loop: a documented evaluation limit bounds
  cascading changes and the limit is tested.

### W7.15 Repository insights

- **Deliverable:** Contributors (commits per author over a window), commit activity (weekly
  series), code frequency (additions and deletions), and dependency graph (manifests detected
  in the default branch with their declared dependencies and known-manifest formats),
  presented with explicit empty and partial states. Traffic (views and clones) is collected
  only from a documented first-party counter with retention and no third-party analytics.
- **Depends on:** Phase 1 repository storage, W7.1
- **Acceptance:** Insights require read permission and the denied case is tested. Series data
  is derived from Git history with a documented time window and timezone, and a fixture
  repository produces the expected series. Dependency graph reports only manifests it
  recognizes and lists unrecognized manifest files as unsupported rather than silently
  omitting them. Traffic counters carry a retention limit and are documented as approximate.

### W7.16 Profile pages

- **Deliverable:** User and organization profiles with avatar, display name, bio, links,
  pinned repositories with a visibility filter, contribution activity summary, a
  repositories tab with search and filters, stars, followers, and an organization profile
  listing teams the viewer may see.
- **Depends on:** W7.7, W7.8, Phase 2 W2.10
- **Acceptance:** Pinned items and contribution summaries filter to what the viewer can read,
  and a profile for a user with only private repositories does not disclose repository names.
  Profile content is rendered through the Phase 2 Markdown renderer with raw HTML disabled.
  An empty profile renders a complete empty state rather than a partial page.

### W7.17 Discovery API and web surfaces

- **Deliverable:** `/api/v1` endpoints for search (with qualifiers and cursor pagination),
  topics, stars, watch state, follows, feeds, wikis, pages publish and domain state,
  discussions, projects and items, and insights, following the error, pagination, and
  deprecation contract fixed in W2.17, plus web routes and React views in `web/` for each.
- **Depends on:** W7.2, W7.4, W7.9, W7.13, W7.14, W7.15, W7.16
- **Acceptance:** Every endpoint returns the documented error shape for unauthorized,
  forbidden, not found, conflict, and validation failures, asserted by table-driven tests.
  Response-shape tests fail on a removed field or changed type. Rate limits protect search and
  feed endpoints, and exceeding a limit returns the documented response rather than a
  generic error.

## Data and interfaces

Forward-only migrations appended to `migrations/`. New tables: `search_documents` and
`search_terms` (or the equivalent `tsvector` columns and generated indexes),
`search_index_state` for backfill progress and lag, `topics` and `repository_topics`,
`stars`, `watchers`, `follows`, `feed_events` where the Phase 2 activity table is not
sufficient, wiki metadata (the wiki content itself lives in Git), `pages_sites`,
`pages_deployments`, `pages_domains`, `discussion_categories`, `discussions`,
`discussion_replies`, `discussion_votes`, `polls` and `poll_votes`, `projects`, `project_views`,
`project_items`, `project_fields`, `project_item_values`, `project_automations`, and
`traffic_counters`. Every table that holds discoverable content carries the repository or
organization scope used by permission filtering, with the scope plus ordering column indexed.

API surface under `/api/v1`: `/search` (with a `type` parameter), `/topics`,
`/repositories/{owner}/{repo}/topics`, stars and stargazers, watchers, follows,
`/feeds`, `/repositories/{owner}/{repo}/wiki`, pages deployments and domains,
discussion categories, discussions, replies, votes, answers, polls, projects, views, items,
fields, automations, and insights endpoints. Search responses include an explicit
completeness indicator when the index is degraded or lagging.

Events: `search.reindex_requested`, `search.index_degraded`, `topic.changed`,
`star.added`, `star.removed`, `watch.changed`, `follow.changed`, `pages.published`,
`pages.domain_verified`, `discussion.created`, `discussion.answered`, `poll.voted`,
`project.item_changed`, `project.automation_run`. Events carry stable IDs and are versioned
before external consumers depend on them.

Packages touched: `internal/search`, `internal/pages`, `internal/discussions`,
`internal/projects`, `internal/activity`, `internal/repository`, `internal/issues`,
`internal/pullrequest`, `internal/jobs`, `internal/runner` (Pages build execution),
`internal/platform/{postgres,storage,search,container}`, and
`internal/transport/http/{web,api}` plus the React sources in `web/`.

## Security notes

- Every discovery query filters by permission before ranking or counting. Facet counts,
  totals, and trending lists are treated as disclosure surfaces and are covered by denied-case
  tests, not only the item lists themselves.
- Code search and dependency graph read untrusted repository content. Snippets escape
  content, indexing excludes files over the size cap and binary files, and no repository text
  is interpreted as a query or template.
- Pages serving resolves the target site from the verified host and a stored site record, and
  never from a client-supplied path under the serving root; path traversal, symlinks outside
  the artifact, and directory listing are all rejected and tested.
- The Pages build step runs repository-controlled build commands. It executes through the
  Phase 5 runner boundary with resource limits and no access to the application's database
  credentials or repository storage outside the checkout, or it is disabled entirely when no
  runner is configured.
- Custom domain verification requires proof of control before serving, prevents one domain
  from being claimed twice, and rechecks on renewal so a lapsed domain cannot keep serving.
- Traffic counters and search indexing store no credentials, request bodies, or repository
  file contents beyond indexed snippets, and retention limits are enforced by a job.
- Projects automations are bounded and audited so a hostile project definition cannot cause
  unbounded writes or a change loop.

## Out of scope

Advanced Security (code scanning, secret scanning, Dependabot alerts and updates), Copilot
and AI features including AI search ranking or summaries, the mobile app, GitHub's
Codespaces, Gist (replaced by the cloud-development subsystem), Sponsors and payments, and
marketplace payments and distribution. A dedicated search cluster is introduced only if the
SQL path measurably fails; the commitment here is the port and the conformance suite, not a
particular engine. Object storage for Pages artifacts is an option behind
`internal/platform/storage`, not a requirement. Cross-instance federation and public
timelines beyond follows and explore are deferred.

## Exit criteria

- Search returns correct, permission-filtered results across repositories, code, issues and
  pull requests, commits, users, and topics on the SQL-only baseline, and degrades to that
  baseline when an external index is unavailable.
- Topics, stars, watching, follows, and explore work on a single node with documented
  ranking inputs, and none of them disclose private repository names or counts.
- A repository wiki round-trips through Git and the browser with permission checks in both
  directions.
- A site publishes atomically, serves on a same-origin path by default, and serves a verified
  custom domain with the documented TLS mode while rejecting unverified hostnames.
- Discussions and Projects support their documented workflows with server-side permission
  checks and bounded automations.
- Insights render contributors, commit activity, and a dependency graph with explicit partial
  and empty states, and profile pages never disclose unreadable content.
- The `/api/v1` discovery contract is documented and covered by response-shape and
  permission-denied tests.
