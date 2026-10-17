# Pull requests roadmap

**Phase:** 3
**Status:** not started
**Roadmap file:** docs/roadmap/03-pull-requests.md

## Purpose

Phase 3 covers the reviewed change: comparing refs, rendering diffs, running a pull request
from draft to merge, and collecting reviews with enough context to be trustworthy. The hard
parts are diff safety at scale, review state that cannot be silently invalidated, and merge
correctness when branches move concurrently. Every workstream assumes the Phase 2 permission
services and issue timeline already exist and reuses them rather than adding parallel rules.

## Scope

Ref comparison for branches in one repository and for forks; unified and split diff
rendering with caps for large diffs; pull-request lifecycle (draft, ready for review, close,
reopen); reviews with comment, approve, and request-changes states; review threads and
resolution; suggested changes; outdated-comment tracking; required reviewers; dismissal of
stale approvals on push; merge commit, squash, and rebase merge methods; merge queue;
auto-merge; conflict detection and guidance; linked issues; check-run integration points; and
concurrent-push safety through Git ref transactions or compare-and-swap ref updates.

## Workstreams

### W3.1 Ref resolution and comparison

- **Deliverable:** `internal/pullrequest` comparison service that validates a base and head
  ref against refs that actually exist, supports same-repository branches and fork heads, and
  records the resolved base and head object IDs, the merge base, the commit count, and the
  changed-file summary for a pull request.
- **Depends on:** Phase 2 W2.11 (fork permission resolution)
- **Acceptance:** Nonexistent refs, tag-only refs, and deleted branches fail with a specific
  error and no pull request row. An inaccessible fork returns the same not-found response as
  a missing ref, so private repository names do not leak. The recorded object IDs match
  `git rev-parse` output for the same refs at comparison time.

### W3.2 Diff computation with caps

- **Deliverable:** Bounded diff generation in `internal/platform/git` producing per-file
  patches, additions/deletions counts, rename and mode-change detection, and binary and
  submodule markers, with per-file, per-pull-request, and time limits and a truncation
  marker when a limit is hit.
- **Depends on:** W3.1
- **Acceptance:** A diff larger than the configured cap returns a truncated result plus a
  clear indicator instead of failing or hanging. Mergetag, rename, binary, and symlink cases
  are covered by fixtures. Diff generation never reads beyond the configured object limits
  and cannot be made to consume unbounded memory by a crafted repository.

### W3.3 Diff rendering and split view

- **Deliverable:** React components in `web/` for unified and split diff views: line numbers
  on both sides, hunk headers, whitespace-only change indicator, collapse/expand context,
  file tree navigation, per-file viewed state, and keyboard navigation. Controller-side
  preparation of diff rows so the client does not re-derive alignment.
- **Depends on:** W3.2
- **Acceptance:** Split and unified views show the same hunks and the same line counts for a
  fixture diff. Source text is escaped and never injected as HTML. A pull request at the size
  cap renders within the page budget and the non-JavaScript fallback still shows the patch.
  Keyboard-only navigation reaches every file.

### W3.4 Large-diff and large-pull-request behavior

- **Deliverable:** Server-side caps for the number of files and total diff bytes, per-file
  lazy loading beyond a threshold, a "load more files" path, and consistent behavior for
  web, API, and email-rendered summaries.
- **Depends on:** W3.2, W3.3
- **Acceptance:** A pull request with thousands of changed files returns a first page within
  the response-time budget and never materializes the full diff in one request. The API
  returns the same truncation metadata as the web UI. A truncated diff is labeled as
  truncated rather than presented as complete.

### W3.5 Pull-request creation and lifecycle

- **Deliverable:** Create, draft, ready-for-review, close, reopen, and edit (title, body,
  base ref, reviewers) use cases, with list filters by state, author, assignee, label,
  milestone, and base branch. Title, body, and refs are validated server-side.
- **Depends on:** W3.1
- **Acceptance:** A draft cannot be merged and converting to ready emits a timeline event. A
  pull request cannot be created between a repository and itself with identical refs.
  Changing the base ref re-runs comparison and invalidates diff-version-scoped review data
  according to the documented rule. Closed pull requests keep comments and history readable.

### W3.6 Reviews: comment, approve, request changes

- **Deliverable:** Review submission with a state, an optional general body, and file or line
  comments attached to a specific head object ID. Reviewer eligibility, self-review rules,
  and the list of current review states per reviewer are exposed on the pull request.
- **Depends on:** W3.5
- **Acceptance:** A user without read access cannot submit or read reviews. Self-approval is
  rejected by policy, and self-comment is allowed. The aggregate state (approved, changes
  requested, review required) is derived from current reviews rather than stored twice, and
  is tested for the mix of states including multiple reviewers.

### W3.7 Review threads, comments, and resolution

- **Deliverable:** Threaded line comments anchored to (file path, side, line, diff version),
  reply, edit, delete, resolve and unresolve, and a filter for unresolved threads. Thread
  state is per pull request and visible in the API.
- **Depends on:** W3.6
- **Acceptance:** Resolving a thread requires write access or thread participation as
  documented, and the denied case is tested. Replies keep the thread anchor. Deleting a
  comment inside a thread preserves thread state and never leaves an orphan anchor.
  Concurrent resolve and reply operations do not lose either change.

### W3.8 Outdated comments and diff versions

- **Deliverable:** A diff-version record per pull request capturing base and head object IDs
  plus patch fingerprint, and per-comment anchoring that marks a comment outdated when its
  anchor no longer matches the current diff version, with an "outdated" collapse in the UI
  and a historical view.
- **Depends on:** W3.3, W3.7
- **Acceptance:** Pushing a new commit marks affected comments outdated without deleting
  them, and unaffected comments stay current. A reviewer can still read every outdated
  comment with the code it referred to. Anchor recomputation after a force push does not
  misattribute a comment to a different line without marking it outdated.

### W3.9 Suggested changes

- **Deliverable:** Fenced `suggestion` blocks in review comments parsed server-side, rendered
  as a proposed replacement for the anchored line range, applied on the client into the
  description or a commit the user copies, and validated against the current file content
  and diff version before rendering.
- **Depends on:** W3.7
- **Acceptance:** A suggestion whose anchor or original line range no longer matches is
  marked stale and cannot be presented as directly applicable. Malformed suggestion blocks
  render as ordinary code blocks. Applying multiple suggestions that overlap is refused with
  a clear message.

### W3.10 Required reviewers and review requirements

- **Deliverable:** Repository-level review requirements: minimum approving reviewers, code
  owners or a reviewer list, whether the author counts, and whether stale approvals are
  dismissed. Evaluated by a single service function that returns an explainable result.
- **Depends on:** W3.6
- **Acceptance:** The service returns the missing requirement, not just a false result, and
  the web and API surfaces show the same explanation. Changing the requirement does not
  retroactively satisfy or silently drop existing reviews; the evaluation uses current
  reviews against the current head. Requirements cannot be configured by a user without
  repository admin permission, which is tested as a denied case.

### W3.11 Dismissal of stale reviews on push

- **Deliverable:** A policy and implementation that marks approvals stale when the head
  object ID changes after the approval, records the dismissal reason and actor, and
  optionally re-requests review from the dismissed reviewers. Configurable per repository and
  per review requirement.
- **Depends on:** W3.6, W3.10
- **Acceptance:** An approval given before a push no longer satisfies the requirement after
  the push, and the reviewer list shows why. A push that only updates the base branch does
  not dismiss approvals, per the documented rule. Dismissal is idempotent when the same push
  is processed twice.

### W3.12 Merge commit method

- **Deliverable:** Merge-commit merge in `internal/pullrequest` using a bounded Git command
  sequence: verify permission and refs, create the merge commit with the recorded author and
  message, update the target ref with compare-and-swap, then record the merge result. The
  first and only enabled method.
- **Depends on:** W3.5, W3.10
- **Acceptance:** A permitted merge produces exactly one merge commit with the expected
  parents and the pull request moves to merged. Merging twice, merging a closed pull request,
  merging without write access, and merging when the target ref moved are all refused with
  no unintended ref update. Crash between ref update and database write is recoverable by a
  documented reconciliation path, covered by a test that simulates the interruption.

### W3.13 Squash and rebase merge methods

- **Deliverable:** Squash (single commit with a composed message) and rebase (replay head
  commits onto the base) methods, selectable per repository with an allowed-methods setting
  and per-merge override. Commit authorship, committer identity, and message composition
  rules are documented.
- **Depends on:** W3.12
- **Acceptance:** Squash produces one commit whose tree matches the merge result tree, and
  rebase produces a linear history whose tree matches. Conflicted replay fails without
  touching the target ref and reports the conflicting paths. Disabled methods are refused in
  both web and API paths, and each method is covered by a test asserting the resulting ref
  and commit graph.

### W3.14 Concurrent-push safety

- **Deliverable:** Ref update primitive in `internal/platform/git` that uses
  `git update-ref` with the expected old object ID (compare-and-swap) or a ref transaction
  for multi-ref updates, plus a pre-merge and post-merge verification of the recorded base
  and head IDs.
- **Depends on:** W3.12
- **Acceptance:** A concurrent push between verification and ref update causes the merge to
  fail and report a moved base, leaving the pushed commit intact. Tests run a real
  push-and-merge race and assert the outcome; a retry after a lost race on an unchanged head
  succeeds. Multi-ref operations are all-or-nothing.

### W3.15 Merge queue and auto-merge

- **Deliverable:** Auto-merge as a per-pull-request intent evaluated after each head update,
  plus a per-repository merge queue: entries ordered by insertion, each validated against the
  current target ref, merged on success, and removed with a recorded reason on failure.
- **Depends on:** W3.14, W3.16
- **Acceptance:** Enabling auto-merge on a pull request that satisfies all requirements merges
  it without further user action once requirements are met; disabling it cancels any pending
  action. Queue entries never merge against a stale base, and a failed entry does not block
  later entries. A queue worker restart does not lose or duplicate an entry, asserted by a
  test that stops and resumes the worker.

### W3.16 Checks integration points

- **Deliverable:** A check-run record (name, status, conclusion, details URL, head object ID,
  required flag) with an internal interface and an ingestion endpoint reserved for the Phase
  5 Actions work, plus a mergeability evaluator that combines checks, review requirements,
  and conflicts into one explainable result.
- **Depends on:** W3.10
- **Acceptance:** Checks are attributed to a head object ID, so checks from an older push do
  not count for the current head. Required checks that are missing, pending, or failing block
  the merge and the block reason names the check. A check result cannot be submitted by an
  unauthenticated caller, and the endpoint is covered by allowed and denied tests.

### W3.17 Conflict detection and resolution guidance

- **Deliverable:** Mergeability computation (mergeable, conflicting, unknown while computing)
  with a bounded merge-tree or in-memory merge, the list of conflicting paths, and a help
  path showing the local commands to resolve the conflict. Recomputed on base and head
  updates as a durable job.
- **Depends on:** W3.2
- **Acceptance:** A conflicting fixture reports conflicting with the correct path list and
  never reports a false mergeable while computation is pending. The UI distinguishes unknown
  from conflicting. A conflicting pull request cannot be merged even if reviews and checks
  pass, and the refusal is tested.

### W3.18 Linked issues and closing keywords

- **Deliverable:** Explicit issue linking from the pull-request sidebar plus closing-keyword
  parsing from the pull-request body confined to a documented keyword set, with cross
  references recorded in both timelines. Closing applies only on merge into the default
  branch.
- **Depends on:** Phase 2 W2.6
- **Acceptance:** Keywords inside code blocks or quotes do not close issues. A linked issue in
  another repository closes only when the actor had write access there, otherwise the link is
  recorded without a state change. Merging closes exactly the linked open issues, once, and
  the closed issues name the pull request that closed them.

### W3.19 API and web surface for pull requests

- **Deliverable:** `/api/v1` pull-request endpoints (create, read, update, list, compare,
  diff, files, reviews, review comments, merge, mergeability, auto-merge, queue state) with
  the Phase 2 error and pagination contract, plus web routes for list, detail, files, and
  conversation views.
- **Depends on:** W3.5, W3.7, W3.12, W3.16, W3.17
- **Acceptance:** Response-shape tests fail when a field is removed or its type changes.
  Anonymous callers receive not-found for private repositories on every endpoint. Diff and
  file-list endpoints honor the same caps and truncation metadata as the web UI, asserted by
  comparing both responses for one pull request.

## Data and interfaces

Forward-only migrations appended to `migrations/`. New tables: `pull_requests`,
`pull_request_versions` (base and head object IDs plus patch fingerprint), `reviews`,
`review_comments`, `review_threads`, `review_requirements` and `review_requirement_reviewers`,
`check_runs`, `merge_queue_entries`, `auto_merge_requests`, and `linked_issues`. Pull
requests share the per-repository number sequence with issues from Phase 2 so `#123` is
unambiguous; the shared allocation path is a single service function.

API surface under `/api/v1` follows the naming and error contract fixed in W2.17. Merge
endpoints are non-idempotent by nature and therefore require a caller-supplied idempotency
key or an explicit head object ID, which is documented in the contract note.

Events: `pull_request.opened`, `pull_request.ready_for_review`, `pull_request.closed`,
`pull_request.reopened`, `pull_request.synchronize`, `pull_request.review_submitted`,
`pull_request.review_dismissed`, `pull_request.thread_resolved`, `pull_request.merged`,
`pull_request.auto_merge_enabled`, `pull_request.auto_merge_disabled`. Each event carries the
base and head object IDs it was computed against.

Packages touched: `internal/pullrequest`, `internal/issues` (number allocation, cross
references), `internal/repository` (review requirements and merge settings),
`internal/gittransport` (ref reads and updates through one path), `internal/jobs` (diff
refresh, mergeability, queue), `internal/activity` (timeline events),
`internal/platform/git`, `internal/platform/postgres`, and
`internal/transport/http/{web,api}` plus the React sources in `web/`.

## Security notes

- Merge authorization is checked in the pull-request service immediately before the ref
  update, not at page render time. A permission revoked between render and merge must fail
  the merge, and the test suite covers that case.
- Diff and blob content is untrusted. Rendering escapes all source text, caps output size,
  and never evaluates repository content. Submodule and symlink entries are displayed as
  metadata and never followed.
- Fork pull requests do not grant the fork author any access to the base repository, and
  comparison must not disclose base-repository private content to a fork the viewer cannot
  read.
- Ref updates use compare-and-swap or ref transactions. There is no code path that force
  updates a branch as part of a merge, and no repository-provided hook is executed.
- Check ingestion authenticates the submitter and binds results to a head object ID so an
  older passing run cannot be replayed to satisfy a requirement on new code.
- Merge queue and auto-merge workers re-evaluate permission and requirements at merge time
  and treat their earlier evaluation as advisory only.

## Out of scope

Advanced Security (code scanning, secret scanning, Dependabot alerts and updates),
Copilot and AI-assisted review or summaries, the mobile app, GitHub's Codespaces, Gist,
Sponsors and payments, and marketplace distribution. The Actions engine that produces check
runs is Phase 5; this phase defines only the ingestion contract and the mergeability
evaluation. Stacked pull requests, merge trains across multiple pull requests, and
code-owner file syntax beyond a simple reviewer list are deferred until the core merge path
is proven.

## Exit criteria

- A developer can push a branch, open a draft pull request, mark it ready, and see an
  accurate diff with correct large-diff behavior.
- Reviewers can comment on lines, reply in threads, approve or request changes, and see
  comments correctly marked outdated after a new push.
- Required reviewers, dismissed stale approvals, required checks, and conflicts all block a
  merge with an explanation that names the failing requirement.
- Merge commit, squash, and rebase are selectable and produce the documented commit graph; a
  concurrent push during a merge can never overwrite the pushed commit.
- Auto-merge and the merge queue merge exactly once per pull request, survive a worker
  restart, and refuse stale bases.
- The `/api/v1` pull-request contract is documented, covered by response-shape tests, and
  consistent with the Phase 2 error and pagination rules.
