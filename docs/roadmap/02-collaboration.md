# Collaboration roadmap

**Phase:** 2
**Status:** not started
**Roadmap file:** docs/roadmap/02-collaboration.md

## Purpose

Phase 2 turns repository hosting into team collaboration: issues, comments, labels and
milestones, organizations with teams and inherited permissions, durable notifications, an
audit trail, and Git over SSH. It also freezes the `/api/v1` contract for these surfaces so
automation built on them does not break later. Every workstream keeps authorization inside
shared application services and treats user text, Git input, and email delivery as untrusted.

## Scope

Issues and their metadata (labels, milestones, assignees, templates, task lists, timeline,
reactions, transfer between repositories), issue comments, and safe Markdown rendering.
Organizations, teams, nested teams, outside collaborators, membership roles, permission
inheritance, and repository transfer between owners. Issue and pull-request subscriptions,
the notification inbox and threads, mute, and email delivery through a durable outbox. Audit
and activity events. SSH Git transport with key registration, fingerprints, revocation, and
restricted command dispatch. API stability rules for the surfaces introduced here.

## Workstreams

### W2.1 Markdown rendering with raw HTML disabled

- **Status:** implemented
- **Deliverable:** A rendering package used by every description, comment, wiki-less text
  field, and email body. CommonMark plus tables, task lists, strikethrough, autolinks, and
  fenced code. Raw HTML is not passed through; the sanitizer allowlist is the only source of
  permitted elements and attributes.
- **Depends on:** None
- **Delivered:** `internal/markdown` — `markdown.go` (the parser, the escaping renderer, the
  memoizing cache) and `policy.go` (the allowlist) — wired into the repository overview so a
  Markdown README is rendered, and into `internal/transport/http/web` as the one renderer
  every read surface shares.
- **Ticked in this change, with the evidence:**
  - `Markdown rendering with tables, task lists, strikethrough, autolinks, and fenced code`.
    Each of the five named features has an assertion in `TestTheAllowedElementsSurvive`, and
    all five appear in the golden fixture.
  - `Raw HTML disabled in all rendered user text`. The corpus is 74 payloads across
    `<script>`, `<iframe>`, `<object>`, `<embed>`, `<style>`, `<base>`, `<meta>`, `<form>`,
    `<link>`, SVG and MathML script contexts, event handlers in six spellings, `javascript:` /
    `data:` / `vbscript:` / `file:` targets in seven obfuscations, conditional comments, and
    parser-confusing shapes. The renderer is the only path by which user text becomes HTML,
    which is what "all" rests on: a later workstream must route its text through it rather
    than adding a second renderer.
- **Notes:**
  - **The escape-versus-drop decision, which the acceptance criteria permit either way.** A
    document containing `<div>` renders the visible text `&lt;div&gt;`. goldmark's default is a
    comment saying "raw HTML omitted", which the sanitizer's comment-stripping then removes —
    leaving a reader with nothing where their text was. Escaping is both truthful and inert, so
    it is what this package does, stated once in `escapedHTML`'s comment so a future reader does
    not "fix" it back.
  - **Both controls are implemented, and they do different jobs.** The parser never treats
    input as HTML; the sanitizer runs on the parser's output and its allowlist is the only
    source of permitted elements. That makes adding a parser extension safe: a new element is
    stripped until it is named in `policy.go`, deliberately.
  - **The sanitizer is bluemonday and the parser is goldmark, rather than hand-written.**
    Writing an HTML sanitizer is a known way to ship a vulnerability, and the documents name
    a sanitizer as the control. The policy is built as an **allowlist from nothing** —
    `AllowStandardURLs` and `AllowStandardAttributes` are deliberately not used, because they
    grant a set nobody in this repository wrote down and therefore nobody reviews.
  - **What the sanitizer does *not* do, stated because it is a decision.** An `<img>` may
    carry an absolute URL to another host, so a document could make a reader's browser fetch
    a remote resource. Restricting that is a **fetch** control, and it is enforced where the
    fetch happens: the instance sets `Content-Security-Policy: img-src 'self'`, so a browser
    will not load a remote image from any page this process serves. T-B6 calls for a
    "documented allowlist of image hosts or proxied through GitNest", which is the
    refinement and needs an image proxy that does not exist yet.
  - **The cache is bounded and keyed by version, and it is not single-flight.**
    `docs/security/threat-model.md` T-L8 names the control as "single-flight **or** jittered
    cache fills with **bounded entry counts**"; the bounded half is implemented — a
    least-recently-used bound and a per-entry byte bound, so one large document cannot
    displace every small one. Two goroutines that miss the same key do both render. The test
    asserts that behaviour rather than claiming coalescing that is not there, and records
    what single-flight would buy.
  - **Two defects the tests found, both real.**
    First, the escaping renderer **panicked** on a raw HTML block with no closing tag: the
    zero-valued closure segment indexes outside the source, and four corpus payloads are
    exactly that shape, so the guard on `HasClosure()` is what makes them renderable.
    Second, rendering a README verbatim **put a second `<h1>` on every repository overview**,
    which the accessibility test caught. Documents rendered inside a page are now demoted one
    heading level, so the repository name remains the page's only first-level heading, and
    the shift is part of the cache key so the two modes cannot be confused.
  - **The danger checks run on the parsed tree, not on the rendered text.** The first version
    searched the output for `onerror=` and similar, which produced only false positives:
    escaped text legitimately contains those characters. Parsing the output the way a browser
    does and inspecting every node and attribute is both the correct question and the
    stricter check, because a payload surviving in a position the corpus did not anticipate
    is still caught.
- **Verification:** 88 assertions across the package, zero skips, including the 74-case
  corpus and the golden file. The full suite passes with `-race` against a real database
  across 20 packages, and in no-database mode.

### W2.2 Issue model and lifecycle

- **Status:** split into W2.2a, W2.2b, and W2.2c. **All three are implemented.**
- **Deliverable:** `internal/issues` domain: per-repository issue numbers allocated in the
  same transaction as repository row locking, title, body, author, state, `closed_at`,
  `closed_by`, and immutable `created_at`/`updated_at`. Create, view, edit title/body, close,
  reopen, and delete-with-audit use cases.
- **Depends on:** W2.1
- **Acceptance:** Numbers are contiguous per repository with no gaps from failed
  transactions; two concurrent creates never share a number. Closed and reopened issues keep
  a state-change history. Reading an issue requires repository read permission; closing,
  editing, and reopening require write permission or issue author rules, and the denied case
  is tested. Issue counts for a private repository are not exposed to anonymous callers.
- **Why it is split:** the deliverable names the *domain* and the *use cases* separately, and
  the acceptance criteria divide cleanly along the same line. Everything about numbering,
  authorization, and the state history is a property of the domain and is verifiable without
  a transport; a surface is a second reviewable change that needs its own routes, response
  shapes, and read pages.

#### W2.2a Issue domain, numbering, and lifecycle rules
- **Status:** implemented
- **Delivered:** `migrations/0007_issues.up.sql`, `internal/issues` (`issues.go` for the model
  and validation, `service.go` for the use cases and every access decision, `postgres.go` for
  the store), and the four `issue.*` actions in `internal/audit`.
- **Ticked in this change:** `Issue counts that never leak a private repository's numbers`.
  Evidence: `Count` and `List` both go through the same access decision as a read, so an
  anonymous caller is refused as not-found before any query runs. A database-backed test
  asserts an anonymous list, count, and read are all refused, and that a collaborator holding
  read sees the count — which is what makes the refusals a decision rather than a broken
  query.
- **Notes:**
  - **The numbering mechanism is the load-bearing decision, and three constraints chose it.**
    `data-model.md` requires numbers to be "never reused, including after deletion"; W2.2's
    acceptance requires "no gaps from failed transactions"; and the same acceptance requires
    that "two concurrent creates never share a number". A `SEQUENCE` satisfies the third and
    violates the second, because `nextval` is not transactional. A read-then-write violates the
    third. So the counter is a **column on the repository row**, allocated with an atomic
    `UPDATE ... RETURNING` inside the creating transaction — the mechanism the data-model
    concurrency table names, "Atomic `UPDATE ... SET n = n + 1`, never read-modify-write". The
    cost is that issue creation serializes per repository, which is the right granularity: two
    repositories never contend.
  - **The counter sits on `repositories`, not on `issues`, because the number is shared with
    pull requests.** `data-model.md` says the two "share one per-repository sequence and are
    never reused", so phase 3 draws from the same column rather than needing a second scheme to
    reconcile. A test asserts a second repository has its own counter starting at 1.
  - **Both claims that needed a real database are asserted against one.** `refuses no number`
    forces a failure **after** the allocation — a primary-key collision — because that is the
    window the claim is about, and asserts the counter did not move, the next issue takes the
    number the failure would have used, and the ledger holds no record of the change that did
    not happen. `two concurrent creates never share a number` runs 24 creations released by one
    channel and asserts the numbers come out as exactly 1..24.
  - **The state-change history is the audit ledger, and no second history exists.** The
    catalogue defines `issue.created`, `issue.closed`, `issue.reopened`, `issue.deleted`, and
    the ledger is append-only, already records the actor and the moment, and has **no foreign
    key to the issue** — so a history outlives a deleted issue, which a cascading table would
    not. This migration also widens `audit_events_scope_type_check` to admit `issue`.
  - **An edit writes no audit record, and that is the catalogue's decision.** There is no
    `issue.edited`, so the store method for an edit takes no record at all — the omission
    cannot be mistaken for an oversight in the code. Inventing the spelling would put a value
    into the contract that W2.15, which owns phase 2's event surface, has not agreed to.
  - **The author allowance is scoped to one issue and is not a permission model.** An author
    holding only read may edit, close, and reopen their own issue; they may **not** delete it,
    because removing the record of a discussion is a repository-level act. A test asserts both
    halves, including that a deleted author's issue gives the allowance to nobody.
  - **The two refusals a repository can produce are mapped differently, and getting it wrong
    was a real defect.** The repository service reports not-found when the caller has no access
    and forbidden when the level is merely too low. Collapsing both into not-found made a
    reader's delete claim "the issue does not exist", which is false about a repository they can
    see. Not-found stays not-found, which is the T-A4 shape; forbidden stays forbidden.
  - **Optimistic concurrency, checked where it cannot race.** An edit names the version it read,
    and the guard is in the `UPDATE`'s `WHERE` clause rather than in a read before it. A
    mismatch is `ErrConflict`, and the test asserts the first writer's value survived rather
    than being overwritten.
  - **A bug the round-trip test caught.** The reopen statement carried a `$4` placeholder it
    never supplied — the reopen clears `closed_by` rather than setting it — so PostgreSQL
    reported "could not determine data type of parameter $4". Only a reopen against the real
    database reaches that path, which is why the database-backed suite exists.
  - **Retention is stated in the migration**, as the data-model rule requires: an issue row is
    bounded by the repository that owns it, so there is no time-based window, and deleting an
    issue is a hard delete whose record survives in the ledger.
- **Verification:** 43 assertions across the package, zero skips — 31 against a fake store for
  the rules, 12 against a migrated schema for the numbering, concurrency, and history. The full
  suite passes with `-race` across 21 packages.

#### W2.2b Issue API and server-rendered read surfaces
- **Status:** implemented
- **Deliverable:** the REST resources and the rendered read pages for the operations W2.2a
  implements — `/{owner}/{repo}/issues`, `/issues/{number}`, and the corresponding
  `/api/v1/repos/{owner}/{repo}/issues` shapes — plus the Markdown rendering of a body through
  W2.1's renderer.
- **Depends on:** W2.2a, W2.1
- **Delivered:** `internal/transport/http/api/issues.go` (the six endpoints),
  `internal/transport/http/web/issues.go` (the list and detail pages),
  `internal/transport/http/web/templates/issues.html` and `issue.html`, the issue keys in
  `internal/i18n`, `CreatedAt` on `audit.Entry` so a history can be dated, and the wiring in
  `internal/app` that shares one issue service between the API and the pages.
- **Notes:**
  - **The transport decides nothing, and the repository resolution asks for no level.** Every
    handler resolves the repository at `repository.LevelNone` — which `Level.Allows` treats as
    satisfied by every level, so it enforces nothing — and then hands the decision to the issue
    service, which calls the same `repository.Service` the browser and the Git transports call.
    A transport that enforced a level here would be a second authorization implementation, which
    the architecture forbids.
  - **No internal identifier appears in a response.** `data-model.md` requires that "a stable
    internal UUID never appears in a URL, a redirect target, a storage path, or a user-visible
    payload". A test asserts that none of the issue's, repository's, or storage identifier appears
    anywhere in a payload — including the nested author object, which names a login rather than
    an identifier.
  - **An edit requires the version it read, and that deviates from GitHub deliberately.** GitHub's
    `PATCH` is last-write-wins; this one answers `409` when the version has moved, because
    `data-model.md` requires "optimistic concurrency with a version column or `updated_at`
    comparison" and that "a conflict is a visible error, not a silent overwrite". A `PATCH` that
    changes the title or body without naming a version is a `422` rather than an unguarded write.
  - **A `PATCH` applies the state transition before the text edit**, because a transition bumps the
    version: applying an edit against the client's pre-transition version would conflict with our
    own change. Naming the state an issue is already in is a no-op rather than an error, so a
    client that sends the whole object back is not refused for agreeing with us.
  - **A malformed or unknown number is a `404`, not a `400`.** `/issues/abc` and `/issues/999999`
    are the same thing to a caller — no such issue — and a `400` would distinguish "malformed"
    from "absent", which is a distinction only the server's parser cares about.
  - **`403` and `404` are still different, and the difference is who asked.** A caller whose
    repository level was resolved can already read the repository, so telling them their level is
    too low discloses nothing; a caller with no access at all is refused as not-found, which is
    the T-A4 shape. An end-to-end test asserts the API and the page give the *same* answer for the
    same caller, which is the architectural rule observed from outside.
  - **The list pages by an opaque cursor, not a page number.** The store paginates by keyset, so a
    page number would force it to walk from the beginning — and a row inserted while a reader
    pages would shift every later page, repeating or skipping one. The `more` link therefore
    carries `(created_at, id)`. The API's `page`/`per_page` parameters are honoured on top of that
    by walking the cursor, which is correct and is documented at the call site.
  - **A defect the end-to-end test caught, and the domain test could not.** The detail page's
    history decoded `detail["state"]` with a type assertion against `audit.Change` — but the
    detail is JSON, so it arrives as a `map[string]any` and the assertion never matched. Every
    history rendered without its transition. The domain test asserted the record's shape and
    passed; only a test that observed what a *reader is shown* found it. Both shapes are now
    accepted, and the reasoning is recorded where the decoding happens.
- **Verification:** 293 assertions across `internal/issues`, `internal/transport/http/api`, and
  `internal/transport/http/web`, zero failures, plus five app-level end-to-end tests that drive a
  real server — creating an issue through the API and reading it from the page, closing and
  reopening it and finding both transitions in the rendered history, and asserting the two
  surfaces agree about refusal. The full suite passes with `-race` across 21 packages.
- **Not delivered, and why each outstanding parity line stays unticked.** The read surfaces and the
  API are complete, but **no browser *action* exists yet**: the interactive bundle under `web/src`
  has create, settings, and sign-in features and no issue surface, so a user cannot open, edit,
  close, or delete an issue from a page. That is the missing piece in every remaining issue
  lifecycle line, and it is why none of them is ticked. Two lines need more than that:
  - `View an issue with its full timeline and metadata sidebar` needs comments (W2.3), labels and
    milestones (W2.4), and assignees (W2.5) before a timeline or a sidebar exists to show.
  - `Edit issue title and body with an append-only revision record` **cannot be satisfied as
    written**: `docs/reference/events.md` defines no `issue.edited`, so no revision is recorded.
    The parity line and the event catalogue contradict each other, and the resolution belongs to
    W2.15, which owns phase 2's event surface. Recording an edit either needs a catalogue addition
    or the parity line needs rewording; either is a contract decision rather than an
    implementation choice, so it is raised rather than silently decided.
  - `Issue list with filters for state, label, milestone, assignee, author, and sort` has the state
    filter only; the others need the metadata workstreams.

#### W2.2c Issue mutation surface
- **Status:** implemented
- **Deliverable:** the creation form, the edit form, the close and reopen controls, and the delete
  control, calling the shared issue service with the session's CSRF token.
- **Depends on:** W2.2b
- **Delivered:** `internal/transport/http/web/issue_forms.go` (the handlers, the guards, and the
  message mapping), `templates/issue_new.html`, the controls added to `templates/issue.html`, the
  issue form keys in `internal/i18n`, `issues.MaxBodyBytes` for a request reader's bound, and the
  wiring in `internal/app`.
- **Ticked in this change, with the evidence:**
  - `Create an issue with title and Markdown body` — `TestCreatingAnIssueFromThePage` drives the
    page: the list offers the link, the form loads and carries a token, submitting it answers `303`,
    and the issue page renders the body's Markdown rather than its source.
  - `Close and reopen an issue with state-change history` — `TestClosingAndReopeningFromThePage`
    submits each control and then reads the history, which names `issue.created`, `issue.closed`,
    and `issue.reopened` and shows `open → closed` and `closed → open`.
  - `Delete an issue with audit` — `TestDeletingAnIssueFromThePage` deletes from the page, asserts
    the redirect goes to the list rather than to an issue that no longer exists, and asserts the
    issue afterwards answers `404`. The **audit** half is asserted by `TestADeletionKeepsItsRecord`
    in `internal/issues`, which reads the ledger for the deleted issue's identifier and finds the
    record, with the title it removed, still there after the row is gone.
- **Notes:**
  - **The surface is server-rendered forms, not a React island, and `frontend.md` decides it.**
    The document states the principle — **"A page must be usable before hydration. The React
    islands enhance a working page; they do not build it"** — and the issue detail page is
    already server-rendered and already owns its path. Three properties follow: every operation
    works with scripting disabled, which delivers "every workflow is operable by keyboard alone"
    without depending on a component library; the version guard survives the round trip, because
    the form carries the version it read in a hidden field; and **the workstream is verifiable in
    this environment**, which a React feature would not be, because Vite and Vitest cannot spawn
    esbuild under the sandbox. The previous plan recorded a React feature as the next step; this
    is a deliberate change of approach with a documented reason rather than a drift.
  - **One `POST` path per resource, with the operation named in a field.** An HTML form can send
    only `GET` and `POST`, so a deletion needs either a method a form cannot send or an action
    that names the operation. The second is chosen because it keeps every mutation at the
    **documented** paths: `urls-and-identifiers.md` section 2.4 lists
    `/{owner}/{repo}/issues` and `/{owner}/{repo}/issues/{number}`, and inventing
    `/issues/{number}/close` would add paths the document does not have.
  - **The token is verified before the body is parsed**, and the comparison is the same
    constant-time `identity.DigestsMatch` the API uses rather than a second implementation. A
    form submits the whole body, so the token arrives as a field where the API takes a header —
    only the carrier differs.
  - **A refusal re-renders with the reader's own text.** A creation refused for a title keeps the
    body, and a conflicting edit shows what is *now stored* beside the message saying the reader's
    change was not applied — which is what makes the refusal actionable rather than merely
    reported. Two saves would otherwise have been one.
  - **The controls are predicted from the shared decision, not guessed.** `changeFacts` reads the
    repository level the read already resolved and the issue's own author, so a reader who may not
    change an issue sees the issue without a form rather than a form that will be refused.
    Deletion is narrower than editing, because an author may edit and close their issue but not
    remove the record of the discussion.
  - **Three defects the tests caught.**
    First, the creation form's page data **omitted a field the template named** — a missing field is
    a template *execution* error, so the whole page fell through to the 503 operational-failure
    page. Only a test that rendered the form found it; parsing the template was not enough.
    Second, a **field-only refusal announced nothing**: a title that was too long produced no
    form-level message, so a screen-reader user was told nothing at all. `frontend.md` requires an
    error to be both announced *and* tied to its field, and the summary now exists whenever
    anything failed.
    Third, an assertion in the end-to-end test **asserted nothing** — it posted a form and closed
    the response. It was removed rather than left in, because a test that cannot fail is worse than
    no test.
  - **An unticked box was corrected in the same change.** `Per-repository shared number sequence
    with no gaps and no reuse` was satisfied by W2.2a and left unticked; the four tests that
    establish it are named in the parity evidence. Leaving working code unticked is as wrong as
    ticking missing code, and it was found by re-reading the block rather than by a failure.
- **Verification:** 12 form tests in `internal/transport/http/web` covering each allowed and denied
  path, and eight app-level tests that drive a real server through each flow. The full suite passes
  with `-race` across 21 packages.

### W2.3 Issue comments and edit history

- **Status:** implemented
- **Deliverable:** Comment create, edit, and delete use cases with author and
  repository-write authorization, rendered body, and an append-only revision record for
  edits. Comment ordering is stable by creation time plus ID.
- **Depends on:** W2.1, W2.2
- **Acceptance:** A user without read access cannot list or fetch comments, and cannot infer
  comment counts. Editing a comment records the previous body and does not rewrite history
  visible in the UI. Deletion is soft or audited and never leaves a dangling timeline entry.
- **Delivered:** `migrations/0008_issue_comments.up.sql` (the two tables and the ledger's widened
  scope list), `internal/comments` (`comments.go` for the model and bounds, `service.go` for the use
  cases and every access decision, `postgres.go` for the store), `internal/transport/http/api/comments.go`,
  the conversation section in `internal/transport/http/web/comment_forms.go` and
  `templates/issue.html`, the comment keys in `internal/i18n`, `ActionCommentDeleted` in
  `internal/audit`, and the wiring in `internal/app`.
- **Ticked in this change, with the evidence:**
  - `Comments with Markdown, edit history, and delete-with-audit` — `TestACommentTravelsFromThePageThroughTheAPI`
    writes a comment through the page's form and reads it back through the API and the page;
    `TestEditingACommentShowsItsHistory` edits it and asserts the **previous body is visible in the UI**
    as well as in `GET .../history`; `TestDeletingACommentLeavesATombstoneAndAudits` deletes it and
    asserts the tombstone, the cleared body, and the surviving ledger record.
  - `Stable comment ordering by creation time plus identifier` — `TestTheOrderingIsStableAndTotal` runs
    twelve concurrent creations and asserts the paged walk returns each comment exactly once, with the
    order read back from the database rather than inferred from one query's output.
  - `Comment edit and delete permission rules with denied-case coverage` —
    `TestSomebodyElseNeedsWriteToEdit` asserts a reader who is not the author gets `403` (not
    `404`, because their level was resolved) and that nothing changed; `TestTheAuthorMayEditTheirOwnComment`
    and `TestTheAuthorMayDeleteTheirOwnComment` cover the author allowance, and
    `TestAnUnreadableRepositoryRefusesEveryCommentOperation` covers the no-access case.
- **The record shapes, and why an edit writes no ledger entry.** `docs/reference/events.md` §4.12
  defines exactly one comment action — `comment.deleted` — and no `comment.created` or
  `comment.edited`. **That is consistent rather than a contradiction**, and reading it decided this
  workstream's design:
  - **The audit ledger is the security record**: who acted, on what, when, from where. It has no
    foreign key to the content it describes, which is deliberately what makes it outlive that content.
  - **A revision is domain data**: the previous body, attributed and ordered, which the UI shows as
    "edited". It belongs to the comment and goes when the comment goes, so it is a real foreign key
    with a real cascade.
  So `POST`/`PATCH` write no ledger record and a `DELETE` writes `comment.deleted`. **This also
  resolves the contradiction recorded against the issue parity line** `Edit issue title and body with an
  append-only revision record`: the "revision record" that line names is a table like
  `comment_revisions`, not a catalogue action. Phase 3 should apply the same mechanism to issues, and
  the resolution is noted here rather than left for a maintainer to re-derive.
- **Notes:**
  - **A comment is addressed by number, derived rather than allocated.** `urls-and-identifiers.md` L29
    permits "names, slugs, **numbers**, refs, and content digests" in a URL and forbids a stable
    internal UUID; L159's `{id}` list does not include comments. The number is the comment's rank in
    `(created_at, id)` order, computed by a window function — and it is stable because the order is
    total and a deletion is **soft**, so a row keeps its place. No counter is introduced, and no second
    scheme has to be kept in step with an order that already exists.
  - **A deletion clears the body *and* the revisions in one transaction.** Keeping the text in a
    revision row would make "deleted" mean "hidden from the page" while the words sat one query away.
    The row itself remains, so the conversation keeps its shape and the count agrees with the page.
  - **`comment.deleted`'s detail names the issue**, because the record outlives the text it describes
    and an operator asking "what was removed from this conversation" needs somewhere to look.
  - **Four defects the tests caught, and each needed a different test to find.**
    First, **the API's status mapping had no comment errors at all**, so every deliberate refusal became
    a `500` — a unit test of the comment service could not find it, because the service was behaving
    correctly and only the mapping was absent.
    Second, **the comment forms posted to the issue's own path**, so a comment submission reached the
    *issue* update handler and was refused as an unknown action; the two handlers need different paths
    and the form now takes its action from the comment section.
    Third, **`GetByNumber`'s outer projection named a column the subquery did not produce** and put the
    derived number first while `scanComment` reads it last, so every by-number read failed. **Its
    absence of coverage was the real problem**, so `TestReadingByNumberAgainstTheDatabase` was added —
    and it immediately caught the second half of the bug, which is exactly what it was written for.
    Fourth, **the comment body was escaped twice**: converting the renderer's `template.HTML` to a
    `string` lost the marker that says the sanitizer ran, so the template escaped it again and the page
    showed sanitized markup as visible text. Safe, and useless — which is why an assertion about what a
    reader *sees* found it rather than a security test.
  - **A flaky test fake was fixed rather than tolerated.** The fake counted a comment's position by
    iterating a Go map, whose order is randomised, so a creation and a read could disagree and the
    suite failed intermittently. It now sorts deterministically, and the fix was verified by running the
    affected tests repeatedly — a fake that is only sometimes right teaches a reader to distrust the
    suite.
- **Not delivered, and why.** The flat `GET /issues/comments/{comment_id}` that
  `urls-and-identifiers.md` §3 lists is **not registered**, because it needs an answer the documents do
  not give: with no issue in the path, a number must be unique across the repository, which means either
  a repository-scoped comment counter (a schema decision, and the issue numbering work put its counter
  on the repository for the same reason) or a global per-instance identifier, which is what GitHub uses.
  Both are contract decisions, so the path is left unregistered and the question is raised rather than
  settled by inventing a scheme.
- **Verification:** 22 assertions in the service suite, 13 against a migrated schema, 148 in the web
  package, and seven app-level end-to-end tests that drive a real server. The full suite passes with
  `-race` across 22 packages, in no-database mode, and against a freshly migrated database.

### W2.4 Labels and milestones

- **Status:** split into W2.4a and W2.4b. **Both are implemented.**
- **Deliverable:** Label CRUD per repository with name, color, and description, plus
  apply/remove on issues and pull requests. Milestone CRUD with title, description, due date,
  open/closed state, and progress counts derived from attached issues. Issue list filters for
  label and milestone.
- **Depends on:** W2.2
- **Acceptance:** Label names are unique per repository after case folding. Closed milestones
  reject new attachments. Progress counts stay correct after an issue is closed, reopened, or
  relabeled. Deleting a label removes its attachments without deleting issues.
- **Why it is split:** the deliverable names two entities, and their acceptance criteria do not
  overlap at all — the first two sentences of the acceptance are about labels and the last two
  about milestones. Each is a migration, a domain package, an API surface, and a page, so
  delivering them together would be one very large change rather than one reviewable one.

#### W2.4a Labels
- **Status:** implemented
- **Delivered:** `migrations/0009_labels.up.sql` (the label and attachment tables, and the ledger's
  widened scope list), `internal/labels` (`labels.go` for the model and the two grammars,
  `service.go` for the use cases and every access decision, `postgres.go` for the store),
  `internal/transport/http/api/labels.go`, `templates/labels.html` and the label section of
  `templates/issue.html` with `internal/transport/http/web/labels.go`, the label keys in
  `internal/i18n`, `ActionLabelCreated`/`ActionLabelDeleted` in `internal/audit`, and the wiring in
  `internal/app`.
- **Ticked in this change, with the evidence:**
  - `Labels with name, color, and description, unique per repository after case folding` —
    `TestCreatingALabelFromThePage` creates one through the page's form and asserts the list, the API
    payload, and the refusal of a case variant; `TestTheConstraintDecidesANameConflict` proves the
    **constraint** decides it, including eight concurrent creates of one name where exactly one wins.
  - `Applying and removing labels on issues and pull requests` — **only the issue half is ticked, and
    the line is therefore left unticked.** See the note below.
- **The two levels, and how each was derived rather than chosen.** The catalogue's actor column is
  descriptive prose rather than the actor-kind enum, so the level each operation needs had to be worked
  out. Comparing the rows that say `admin` with the rows that say `user` gives the answer, and W1.10b's
  code confirms it:
  - `repo.renamed`, `repo.visibility_changed`, `repo.archived`, `repo.default_branch_changed`, and
    `repo.issues_toggled` all say `admin`, and **every one is gated by `LevelAdmin`** in
    `internal/repository`.
  - `repo.created` and `repo.forked` say `user`, and both are the operations that *create* a repository
    — where the actor becomes its admin rather than needing to be one.
  - `label.created` / `label.deleted` say `admin`, so **managing a label requires repository admin**: a
    label set is repository configuration rather than content.
  - **Attaching a label to an issue requires repository write**, which is deliberately lower: that
    changes an issue's metadata rather than the repository's configuration, and write is the level that
    already guards an issue's own fields.
  - **No author allowance is given, and its absence is deliberate.** W2.2's acceptance explicitly
    requires "write permission **or issue author rules**" for issue changes; W2.4's says nothing of the
    kind. So this does not invent an allowance the documents do not describe.
- **Notes:**
  - **Case-folded uniqueness is a stored column, folded with `identifier.Fold`.** §6.1 fixes the rule as
    "compared after **Unicode-free ASCII case folding**", and the helper the repository and account names
    already use is exactly that. `lower()` would be wrong: it is Unicode- and collation-dependent, so the
    index, a Go comparison, and the database would not necessarily agree. Because the folding is
    ASCII-only, `Ünïcödé` and `ünïcödé` are **two** labels rather than a collision — which is what
    "Unicode-free" means, and a test asserts it so the rule is not mistaken for an oversight.
  - **The colour grammar is enforced by the database as well as the service.** T-B2 requires that
    "user-controlled colors and identifiers are validated against a grammar, never interpolated raw", and
    §6.5 fixes the grammar as "six hexadecimal digits, no `#` in storage". The CHECK admits **lowercase
    only** and the service normalizes, so the stored alphabet is exactly sixteen characters — which is
    what makes the value safe wherever a read surface places it. A test writes past the service, directly
    into the table, to prove the constraint rather than the normalizer is the guard.
  - **The colour is rendered as text, not as a swatch, and two documented rules decide it.**
    `frontend.md` says both: **"No inline style strings assembled from user data"**, which rules out a
    `style` attribute; and **"Colour is never the only carrier of meaning"**, which rules out making the
    colour the only thing distinguishing two labels. So `#ff0000` appears as the value a reader could
    copy. **This is a deliberate divergence from GitHub's appearance**, taken because the project's own
    rules are explicit — and a test asserts no `style=` attribute appears on the page, so the rule is
    enforced rather than merely intended.
  - **The join table has no surrogate identifier, and that is the project's first exception to the
    `id uuid primary key` rule.** `data-model.md` gives that rule, but an attachment has no identity of
    its own: it is the fact that one label is on one issue. A composite primary key *is* the uniqueness
    constraint — one label cannot be applied twice — and its index serves the only lookup direction the
    table has. Recorded in the migration so the departure is visible.
  - **Two cascades, and each is a stated requirement rather than a convenience.** The join table's
    `label_id` key carries `ON DELETE CASCADE`, so deleting a label removes its attachments and **does
    not touch the issues** — the acceptance criterion, enforced by the schema rather than by a code path
    remembering to be careful. The reverse cascade means deleting an issue also removes its attachments,
    which is what stops a label's issue count from overstating. Both are tested against real tables.
  - **An update writes no ledger record**, because the catalogue defines `label.created` and
    `label.deleted` and no `label.edited`. The store's update method takes no record at all, so the
    omission cannot be mistaken for a forgotten call.
  - **Three defects the tests caught, two of them repeats of earlier ones.**
    First, **the web package's status mapping had no label errors**, so a name conflict answered `422`
    where the API answered `409` — **the second time this exact omission was made**, the comment domain
    having been the first. The fix notes that it is repeated rather than isolated.
    Second, the **label page announced only form-level errors**, so a name conflict — which produces a
    field-level message — announced nothing at all. That is the same defect the issue forms carried.
    Third, a test asserted a colour containing a quote appeared literally in the input's `value`
    attribute; Go's `html/template` correctly escapes it as `&#34;`, so the assertion was rewritten
    against the escaped form **and** now asserts the quote did *not* reach the attribute unescaped, which
    is the property T-B2 actually requires.
  - **A test bug taught something worth writing down.** Reusing one page's CSRF token across four
    submissions failed from the second attempt with `403` — which is `RotateCSRFToken` working as
    designed, since one live token per session is the storage the document prescribes. The test re-reads
    the page each iteration and says why, so it reads as the rotation being exercised rather than as a
    workaround.
- **Not delivered, and why each unticked line stays unticked.**
  - **`Applying and removing labels on issues and pull requests` cannot be ticked**, because it names
    **pull requests**, which arrive in phase 3. The issue half is implemented and tested; the line is a
    conjunction, so it stays unticked until the pull-request half exists. This is the honest reading of
    a line that names two things.
  - `Milestones with title, description, due date, and open/closed state` and
    `Milestone progress counts derived from attached issues` are **W2.4b**, below.
  - `Issue list with filters for state, label, milestone, assignee, author, and sort` needs milestones
    and assignees as well, so it is not finished by this change.
- **Verification:** 35 assertions in the service suite, 13 against a migrated schema including two
  concurrency cases, and eight app-level end-to-end tests that drive a real server. The full suite passes
  with `-race` across 23 packages, in no-database mode, and against a freshly migrated database.

#### W2.4b Milestones
- **Status:** implemented
- **Delivered:** `migrations/0010_milestones.up.sql` (the milestone table, the repository's second counter,
  the issue's milestone column, and the ledger's widened scope list), `internal/milestones`
  (`milestones.go` for the model and grammars, `service.go` for the use cases and every access decision,
  `postgres.go` for the store), `internal/transport/http/api/milestones.go`, `templates/milestones.html`
  and the milestone section of `templates/issue.html` with `internal/transport/http/web/milestones.go`,
  the milestone keys in `internal/i18n`, the two actions in `internal/audit`, the milestone filter on the
  issue list in `internal/issues`, and the wiring in `internal/app`.
- **Ticked in this change, with the evidence:**
  - `Milestones with title, description, due date, and open/closed state` — `TestCreatingAMilestoneFromThePage`
    creates one through the page's form and asserts all four fields on the page and in the API payload, plus
    the case-variant refusal and the due-date grammar.
  - `Milestone progress counts derived from attached issues` — `TestMilestoneProgressIsDerivedFromAttachedIssues`
    attaches three issues, closes one through the issue service, and asserts the count and the percentage move;
    `TestProgressIsDerivedAgainstTheDatabase` asserts the same against real rows and additionally that
    **relabeling an issue does not change them**.
- **The permission level, which is deliberately not the label's.** W2.4a recorded that its derivation would
  not transfer, and this is why. The catalogue says:
  - `repo.renamed`, `repo.visibility_changed`, `repo.archived`, `repo.default_branch_changed`, and the
    feature toggles — **`admin`** — and W1.10b gates each of them with `LevelAdmin`.
  - `label.created` / `label.deleted` — **`admin`**.
  - `milestone.closed` / `milestone.deleted` — **`user`** — and `issue.*`, `comment.deleted` are `user` too.
  So the `admin` rows are **repository configuration** and the `user` rows are **content**, and a milestone sits
  with the content. Changing one therefore requires repository **write**, where managing a *label* requires
  admin. A test asserts both levels in one place, so neither can drift into the other.
- **Notes:**
  - **The number is a counter, not a derived rank, and the requirement is what decides it.** This is the
    difference from a comment, whose number *is* derived — §6.5 and L380 require a milestone number to be
    "numbered per repository in its own sequence, **never reused**", with "the same contiguity and no-reuse
    rules" as issues. A rank would renumber every later milestone the moment one was deleted, which is exactly
    what the rule forbids, so the mechanism is a second counter column on `repositories` allocated with the
    same atomic `UPDATE ... RETURNING`. **A test asserts the difference directly**: it deletes the middle of
    three milestones and requires the third to keep number 3.
  - **An issue has at most one milestone, so the attachment is a column rather than a join table.** An issue
    may carry many labels but only one milestone, so the cardinality differs and the model has to as well: a
    nullable `issues.milestone_id` makes "one" an expressible constraint and the progress a plain join rather
    than a distinct-count. `ON DELETE SET NULL` detaches the issues and does not delete them — the same
    decision the label attachment makes, and the criterion this workstream has to satisfy.
  - **The progress is derived on every read, which is why it cannot go stale.** A stored counter would have to
    be updated by every operation that closes, reopens, attaches, or detaches an issue, and the one that
    forgot would make the number wrong in a way nothing would notice. It is *also* why relabeling cannot
    affect it: labels are a different table, so there is no path by which they could.
  - **A due date is a calendar date, stored as midnight UTC.** The payload catalogue names the field `due_on`
    and a milestone due date is a date rather than an instant — a milestone due "today" is due today wherever
    the reader is. `data-model.md` nevertheless admits no temporal column outside `timestamptz`, so the column
    holds midnight UTC, which is also exactly what GitHub returns for `due_on`. **A timestamp sent where a
    date is expected is refused rather than truncated**, so a client that sends an instant is told the field's
    shape instead of having its value silently changed.
  - **The progress is rendered as text, not as a bar, and "overdue" as a word.** The same two `frontend.md`
    rules that decided the label colour: "No inline style strings assembled from user data" forbids the `width`
    a bar needs, and "Colour is never the only carrier of meaning" would be violated by a bar whose length is
    the only statement of progress. So the page says "1 of 3 issues closed, 33%" and marks an overdue
    milestone with the word. A test asserts no `style=` attribute appears.
  - **Only open milestones are offered as a destination on an issue.** A closed one would refuse the
    attachment, so offering it would be offering a choice that cannot succeed — and the refusal the API
    returns, when a caller reaches it directly, is a **`409`** rather than a `422`, because the request is well
    formed and names a real milestone and it is the milestone's *state* that conflicts. A client can fix that
    by reopening it, which is not true of a malformed request.
  - **Three defects the tests caught.**
    First, **`Close` built its audit record before reading the milestone**, so the record had no scope and
    `audit.Event.Validate` refused it — the store was supposed to fill the scope in, which is too late because
    `change` validates before the transaction opens. The service now reads first and the store no longer fills
    it, which is also what gives the caller a clean not-found and already-in-state refusal.
    Second, **a due date read back was not normalized to UTC**, so a value that was midnight UTC reported
    `Hour() == 8` in a `+08:00` session — the instant was right and its presentation was not. That is the kind
    of difference that makes a comparison or a format depend on the server's timezone, so reads now normalize.
    Third, **`Overdue` compared instants where the rule is about calendar dates**, so a milestone due today was
    reported overdue for the whole of its due date; both sides are now truncated to the start of their UTC day.
    The boundary test is what found it, which is why the clock is injected.
  - **Both status mappings were updated in this change**, which is what the plan asked for after the omission
    had happened twice. The API's `statusFor` and the web's `statusForFormError` each gained the milestone
    domain together, so a milestone conflict answers `409` on a page and in a payload rather than `422` on one
    of them.
- **Verification:** 26 assertions in the service suite, 15 against a migrated schema including three
  concurrency cases and a table-level probe of the writing statements, and nine app-level end-to-end tests that
  drive a real server. The full suite passes with `-race` across 24 packages, in no-database mode, and against
  a freshly migrated database.
- **Not ticked, and why.** `Issue list with filters for state, label, milestone, assignee, author, and sort`
  names **assignees**, which arrive in W2.5, and it names sorting, which no workstream has delivered. The
  milestone half works and is tested; the line is a conjunction, so it stays unticked.
---

> # ⚠️ RECONSTRUCTED SECTIONS — READ THIS FIRST
>
> **Everything from W2.5 to the end of this file was destroyed by a mistake and has been reconstructed
> from surviving evidence.** It is **not** the original text.
>
> **What happened.** In the round that delivered W2.4b, an edit replaced the file with
> `substring(up to the W2.4b heading) + the new W2.4b section`. That silently truncated everything after
> W2.4b: workstreams W2.5–W2.17 and the `Data and interfaces`, `Security notes`, `Out of scope`, and
> `Exit criteria` sections.
>
> **Why it could not be restored at the time.** `docs/` was listed in `.gitignore`, so the directory was
> never committed: `git show HEAD:docs/roadmap/02-collaboration.md` failed, no git object contained the
> string `W2.17`, and no copy existed elsewhere on disk. **That listing has since been removed**, so this
> directory is under version control from here on and a repeat of the mistake would be recoverable.
> **If the maintainer holds the original, it should replace this block wholesale.**
>
> **What the reconstruction is grounded in.** Every claim below is drawn from documents that survived:
> the P2 rows of [parity-checklist.md](../parity-checklist.md), the dependency references in
> [03-pull-requests.md](03-pull-requests.md) and [07-discovery-and-surfaces.md](07-discovery-and-surfaces.md),
> the phase-2 events in [events.md](../reference/events.md), the Collaboration entity map in
> [data-model.md](../architecture/data-model.md), and the route tables in
> [urls-and-identifiers.md](../architecture/urls-and-identifiers.md). The **workstream titles and their
> order are original** — they were recorded before the loss.
>
> **What is inferred rather than original** is flagged in each workstream, and the parity lines whose owner
> could not be established with confidence are listed as **unassigned** at the end rather than guessed at.
> The original prose, the rationale notes, and any cross-cutting guidance those sections carried are gone.

### W2.5 Assignees and issue templates

- **Status:** split into W2.5a and W2.5b. **W2.5a is implemented; W2.5b is not started.**
- **Deliverable:** Assignee management on an issue, restricted to accounts with current read access;
  issue templates read from `.github/ISSUE_TEMPLATE` with a picker at creation, and template
  configuration that routes blank issues to a contact link.
- **Depends on:** W2.2
- **Acceptance:** the parity lines `Multiple assignees limited to users with current read access`,
  `Issue templates from .github/ISSUE_TEMPLATE with a picker`, and `Template config that routes blank
  issues to a contact link`.
- **Why it is split:** the two halves share nothing but a workstream heading. The template half is a
  parser over repository content with no unresolved questions, so it was deliverable; the assignee half
  has a genuine product question (below) that a maintainer has to answer. Delivering the unblocked half
  rather than waiting keeps the phase moving without deciding anything on the maintainer's behalf.

#### W2.5a Issue templates
- **Status:** implemented
- **Delivered:** `internal/issuetemplates` (`templates.go`: the two formats, the configuration, the
  strict decoding, and the catalogue), `gopkg.in/yaml.v3` as a direct dependency, the issue keys in
  `internal/i18n`, and the picker on the issue creation page in
  `internal/transport/http/web/issue_forms.go` and `templates/issue_new.html`.
- **Ticked in this change, with the evidence:**
  - `Issue templates from .github/ISSUE_TEMPLATE with a picker` — 53 assertions covering both formats,
    the metadata, the ordering, and the picker rendering; both the markdown front matter and the YAML
    form are read, including both spellings of the metadata lists that GitHub's own two formats use.
  - `Template config that routes blank issues to a contact link` — the configuration is read, blank
    issues default to enabled, and the contact links are offered when they are disabled.
- **The dependency decision, and why it is an engineering call rather than a product one.** The
  documents require GitHub-compatible template files and fix the parsing discipline — T-H7: "strict YAML
  decoding that rejects duplicate keys, unknown keys, and unsupported syntax" — but name no YAML library.
  Writing a YAML parser that is safe against hostile input is the same class of mistake as writing an
  HTML sanitizer, which W2.1 refused for the same reason, so this uses `yaml.v3` **and asserts in tests
  that it provides exactly the three refusals the documents name** rather than assuming it. `yaml.v3`
  refuses a duplicate key natively and unknown keys under `KnownFields`.
  - The library's own test-dependency graph (`kr/text`, `rogpeppe/go-internal`) appears as **indirect**
    requirements; nothing in this project imports them.
- **Notes:**
  - **The loader never fails, and that is the contract.** A malformed template is a repository author's
    mistake and must not stop everybody else from opening an issue, so every problem lands in
    `Skipped` and the catalogue is always safe to render. A missing `.github/ISSUE_TEMPLATE` directory is
    not even recorded: a repository with no templates is the common case and there is nothing an author
    got wrong.
  - **A refusal is visible rather than silent.** `Skipped` names the file and the reason, so a template
    author can be told which of their files was rejected and why — something an error on the issue page
    could not convey.
  - **A `?template=` value is never used as a path.** It is matched against the names the loader
    discovered, and a test asserts that `../../../etc/passwd`, an absolute path, a `..` segment, a
    `./`-prefixed name, a `\x00`-bearing name, and a case variant all fail to resolve. This is the
    reason the lookup exists rather than a caller reading the file the parameter names.
  - **The contact-link scheme is checked against T-B1's allowlist** — `http`, `https`, `mailto` — and
    that check is a **security control rather than a correctness one**: every rejected value is a
    syntactically valid URL, so contextual escaping cannot distinguish `javascript:alert(1)` from
    `https://example.com`. Without it `config.yml` would be a stored-XSS vector. A test asserts eleven
    executable and malformed schemes are refused and the three allowed ones are accepted.
  - **A malformed configuration keeps blank issues enabled.** The safe direction is the permissive one:
    a file that cannot be understood must not be read as having asked to close the only way a reader can
    still file a report.
  - **The two formats are held to the same rule, and the first version was not.** A form refused an
    oversized metadata list while a markdown template silently truncated one, so the same mistake was an
    error in one file and a quiet surprise in the other. Both now refuse.
  - **A form is marked as one and carries no body.** Its typed `body` elements are not flattened into
    text, because a form is a typed interface rather than a block of text to prefill — so recording the
    distinction is what stops a reader being handed a template that silently does nothing. **This is the
    workstream's known limit**: the picker offers a form and prefills its metadata, but the typed fields
    are not rendered.
  - **A template's body is passed through untouched**, including raw HTML, because escaping and
    sanitizing is W2.1's renderer's job and doing it here would escape a body twice.
  - **Two test defects, both mine and both instructive.** An ordering assertion was written in the case
    I expected rather than the case `sort.Strings` produces — the sort is by UTF-8 bytes, and making it
    case-insensitive would be a third collation rule alongside the label and milestone folds, which are
    ASCII-only for a different purpose. And a boundary test built **exactly** `MaxLabels` labels while
    asserting a refusal, because it sliced `labels[1:]`; it now asserts the count it built and tests both
    sides of the boundary, because a boundary test has to say which side it is on.
- **Verification:** 53 assertions, run under `-race`. The full suite passes with `-race` across 25
  packages and in no-database mode.

#### W2.5b Assignees
- **Status:** not started, and blocked on a product decision rather than on work.
- **Deliverable:** Assignee management on an issue, restricted to accounts with current read access.
- **Depends on:** W2.5a (for the creation page's metadata), W2.2
- **Acceptance:** the parity line `Multiple assignees limited to users with current read access`.
- **⚠️ Three questions the documents do not answer, and they change what a page shows and what an audit
  record must say:**
  1. **When is "current read access" checked** — at assignment, at every read, or both?
  2. **What happens to an existing assignment when the assignee's access is later revoked** — kept and
     hidden, removed, or retained and disclosed? `secrets.md` says a revoked credential "fails on the
     next request", which is about credentials rather than about a stored relationship, so it does not
     settle this.
  3. **May an account with no repository access be assigned at all?** GitHub allows only collaborators
     — a rule *narrower* than "current read access" — which would make the parity line's wording wrong
     rather than the code.
  **Recorded rather than decided.** Each answer implies a different page and a different record, and the
  template half's metadata carries `assignees` through to this workstream, so the decision also fixes
  what a template's `assignees:` key is allowed to do.

### W2.6 Mentions, references, and timeline

- **Status:** not started
- **Deliverable:** `@user` mentions resolved against accounts with read access; `#123` and
  `owner/repo#123` reference resolution with a link table; cross-references recorded in both timelines;
  and the issue timeline itself.
- **Depends on:** W2.1, W2.2, W2.3
- **Acceptance:** the parity lines `@user mentions with notification and access-respecting rendering`,
  `#123 and owner/repo#123 reference resolution with a link table`, and `Cross-references recorded in
  both timelines`. **W2.6 also owns closing keywords**, which
  [03-pull-requests.md](03-pull-requests.md) depends on: "keywords inside code blocks or quotes do not
  close issues", and a linked issue in another repository closes "only when the actor had write access
  there, otherwise the link is recorded without a state change".
- **Note (inferred):** the workstream title names mentions, references, and the timeline; the closing-keyword
  behaviour is established by phase 3's dependency on it rather than by the title.

### W2.7 Reactions

- **Status:** not started
- **Deliverable:** Emoji reactions from a fixed allowlist, with idempotent toggling, on issues and
  comments.
- **Depends on:** W2.2, W2.3
- **Acceptance:** the parity line `Emoji reactions from a fixed allowlist with idempotent toggling`.
- **Note (inferred):** the allowlist is "fixed", so an unknown emoji is refused rather than stored;
  idempotence means toggling twice returns to the original state without a duplicate row.

### W2.8 Task lists and progress

- **Status:** not started
- **Deliverable:** Task lists in issue and comment bodies with a rendered progress summary and an
  endpoint that toggles a checkbox.
- **Depends on:** W2.1, W2.2
- **Acceptance:** the parity line `Task lists with a rendered progress summary and a toggle endpoint`.
- **Note (inferred):** W2.1's renderer already emits disabled checkboxes for a task list, so this
  workstream adds the progress summary, the toggle, and the authorization for who may toggle.

### W2.9 Issue transfer between repositories

- **Status:** not started
- **Deliverable:** Moving an issue to another repository, retaining a record so the old
  `/{owner}/{repo}/issues/{number}` address resolves to the new location with a moved notice, and the old
  number is never reused in the source repository.
- **Depends on:** W2.2
- **Acceptance:** the parity line `Transfer an issue to another repository with a number redirect`, and
  `urls-and-identifiers.md`'s L248 row, which requires that the "old number is never reused in the source".
  The source repository must also have write access by the actor, and the destination read access at
  minimum — a detail the surviving documents state only in the redirect table.

### W2.10 Organizations, membership, and repository ownership

- **Status:** not started
- **Deliverable:** The organization entity, its membership and roles, and organization-owned
  repositories.
- **Depends on:** W2.2, W1.x identity
- **Acceptance:** the parity lines `Create an organization`, `Organization profile with avatar, display
  name, bio, location, and links`, `Organization member list with search`, `Organization roles: owner and
  member`, `Change a member's role, refusing to leave an organization with zero owners`, `Remove a member,
  revoking organization-derived repository access`, `Organization-owned repositories`, `Organization
  repository list with filters`, and `Organization membership granting repository access`.
- **Note (inferred):** [07-discovery-and-surfaces.md](07-discovery-and-surfaces.md) depends on W2.10 for
  organization-scoped surfaces, which is what fixes the boundary at "membership plus ownership".

### W2.11 Teams, nested teams, and outside collaborators

- **Status:** not started
- **Deliverable:** Teams within an organization with members, maintainers, description and visibility;
  nested teams with inherited grants and a cycle guard; grants to repositories with a role; deletion with
  a documented effect on grants; the owners team as a managed default; and outside collaborator
  invitation and acceptance.
- **Depends on:** W2.10
- **Acceptance:** the parity lines `Create a team with a name and derived slug`, `Team description and
  visibility within the organization`, `Team members and maintainers`, `Nested teams with inheritance and
  a cycle guard`, `Team-to-repository grants with a role`, `Nested team inheritance with a cycle guard`,
  `Delete a team with a documented effect on its grants`, `Organization owners team as a managed default`,
  `Outside collaborator invitation and acceptance`, and `Outside collaborator grants confined to granted
  repositories`.
- **Note:** [03-pull-requests.md](03-pull-requests.md) depends on **W2.11 for fork permission
  resolution**, so this workstream also fixes how an outside collaborator's grant and a team grant affect
  a fork's access — the surviving documents state the dependency but not the rule.

### W2.12 Repository transfer between owners

- **Status:** not started
- **Deliverable:** Moving a repository between owners with a target acceptance step, and repository
  templates.
- **Depends on:** W2.10
- **Acceptance:** the parity lines `Transfer a repository between owners with a target acceptance step`
  and `Repository templates: create a new repository from a template`.
- **Note (inferred):** the "target acceptance step" is what distinguishes this from a unilateral move: the
  receiving owner must accept, which is why the line names it.

### W2.13 Notification inbox, threads, and subscriptions

- **Status:** not started
- **Deliverable:** The notification inbox grouped by thread; per-thread subscriptions; mute; read state;
  and a durable fan-out that respects private repository visibility.
- **Depends on:** W2.6, W2.15
- **Acceptance:** the parity lines `Notification inbox grouped by thread`, `Notification reasons (author,
  assigned, mentioned, commented, subscribed, review requested)`, `Per-thread subscription: participating,
  all activity, ignore`, `Mute a thread`, `Mark read, mark all read, and unread filtering`, `Notification
  read state that survives a process restart`, `Durable fan-out that produces one user-visible
  notification per event`, and `Notification fan-out respecting private repository visibility`.
- **Note:** [07-discovery-and-surfaces.md](07-discovery-and-surfaces.md) depends on W2.13 for its
  notification surfaces, which is what fixes the boundary at the inbox and its threads.

### W2.14 Email delivery through a durable outbox

- **Status:** not started
- **Deliverable:** Email delivery through a durable outbox with retry and bounded backoff, per-user email
  settings, a digest option, and signed expiring reply and unsubscribe links.
- **Depends on:** W2.13
- **Acceptance:** the parity lines `Email delivery through a durable outbox with retry and bounded
  backoff`, `Per-user email settings and a digest option`, and `Signed, expiring reply and unsubscribe
  links`.
- **Note (inferred):** "durable outbox" means the send is a job row written in the same transaction as the
  notification, which is the pattern W1.12's job infrastructure established.

### W2.15 Audit and activity events

- **Status:** not started
- **Deliverable:** The audit and activity event surface for collaboration: the event types the catalogue
  tags phase 2, the repository and organization activity feeds, and the shared record the rest of the
  phase writes into.
- **Depends on:** W2.2, W2.10
- **Acceptance:** the parity lines `Repository events feed` and `Fork list with upstream relationship`,
  plus the phase-2 event rows in [events.md](../reference/events.md).
- **Note:** this workstream **owns phase 2's event surface**, which makes it the owner of two open
  questions this file raised earlier: whether the catalogue gains an `issue.edited`/`label.edited` action
  (it currently defines none, so those edits write no record and the parity line
  `Edit issue title and body with an append-only revision record` needs the revision table that W2.3
  established for comments rather than a new action), and
  [07-discovery-and-surfaces.md](07-discovery-and-surfaces.md) depends on W2.15 for its activity surfaces.

### W2.16 SSH Git transport

- **Status:** not started
- **Deliverable:** An SSH entry point with registered keys, fingerprints, and revocation, dispatching only
  `git-upload-pack` and `git-receive-pack`, routing a repository from the requested path, and host keys an
  operator manages.
- **Depends on:** W2.2, W1.x identity
- **Acceptance:** the parity lines `SSH key registration with parsed type and SHA-256 fingerprint`, `SSH
  key duplicate detection, listing, and revocation`, `Key-only SSH entry point with no shell, no PTY, and
  no forwarding`, `Restricted dispatch to git-upload-pack and git-receive-pack only`, `Repository routing
  from the requested SSH path with traversal rejection`, and `SSH host key management by the operator`.
- **Note (inferred):** "no shell, no PTY, and no forwarding" is the `authorized_keys` command restriction
  plus `no-pty`/`no-port-forwarding`, which the parity line states as a behaviour rather than a mechanism.

### W2.17 API stability contract for collaboration surfaces

- **Status:** not started
- **Deliverable:** The naming, error, and deprecation contract for the phase-2 API surfaces.
- **Depends on:** the surfaces it governs
- **Acceptance:** the contract exists and is followed by every collaboration endpoint, and
  [03-pull-requests.md](03-pull-requests.md) can rely on it for its own "naming and error contract" and
  [07-discovery-and-surfaces.md](07-discovery-and-surfaces.md) for its "deprecation contract".
- **Note (inferred):** the deliverable is a document rather than code, which is the only reading that
  fits two independent phase-3 and phase-7 references to it as a fixed contract.

## Data and interfaces

> **Reconstructed as a stub.** The original section is lost. What is known from the surviving documents is
> that it held the phase's schema and interface commitments, which are now carried by:
> [data-model.md](../architecture/data-model.md)'s Collaboration entity group — `issues`,
> `issue_comments`, `labels`, `milestones`, `assignments`, `reactions`, `timeline_events`, `pull_requests`,
> `reviews`, `review_threads`, `review_comments` — and
> [urls-and-identifiers.md](../architecture/urls-and-identifiers.md) sections 2.4 and 3 for the routes and
> API paths. **The migration ledger is the accurate record of what exists:** `0007_issues`, `0008_issue_comments`,
> `0009_labels`, `0010_milestones`.

## Security notes

> **Reconstructed as a stub.** The original section is lost. The phase's security obligations are carried by
> [threat-model.md](../security/threat-model.md), whose T-A (authorization), T-B (rendering) and T-C
> (content) entries name phase-2 workstreams, and by the non-negotiables in
> [product-goals.md](../product-goals.md): no secret in a log, an error, or a metric; no authorization
> question answered outside the shared service; and no untrusted code in the web process.

## Out of scope

> **Reconstructed as a stub.** The original section is lost. The decided exclusions are unchanged and live
> in [product-goals.md](../product-goals.md) and the `Decided exclusions` section of
> [parity-checklist.md](../parity-checklist.md): the mobile app, GitHub's own Codespaces, Advanced
> Security (code scanning, secret scanning, Dependabot), Gist, payments, and AI code features.

## Exit criteria

> **Reconstructed, and this is the weakest part of the recovery.** The original criteria are lost. What can
> be recovered is that they were a phase gate in the same form as phase 1's, and that the phase is judged by
> the `— P2` rows of [parity-checklist.md](../parity-checklist.md), which are the only surviving
> statement of what phase 2 must deliver. **The maintainer should restore the original criteria**; until
> then, treat the P2 rows as the phase's acceptance set.

### Parity lines with no established owner

> **These are listed rather than assigned**, because guessing an owner would put a claim into the roadmap
> that the surviving evidence does not support. Each is a `— P2` line whose workstream could not be
> determined:

- `Issue types or a documented equivalent categorization`
- `Pinned issues on a repository`
- `Saved issue filters per user`
- `Bulk edit of labels, assignees, and milestone on a selection`
- `Issue locking to restrict new comments`
- `Issue list with filters for state, label, milestone, assignee, author, and sort` (the assignee and sort
  halves are outstanding; the state, label, and milestone halves are implemented)
