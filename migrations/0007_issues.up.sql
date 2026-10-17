-- Issues and their per-repository numbering.
--
-- # The numbering rule, which decides the mechanism
--
-- `docs/architecture/data-model.md` states three constraints that together rule out the
-- obvious implementation:
--
--   1. "Issue and pull-request numbers share one per-repository sequence and are never
--      reused, including after deletion."
--   2. W2.2's acceptance: "Numbers are contiguous per repository with **no gaps from failed
--      transactions**."
--   3. The same acceptance: "two concurrent creates never share a number."
--
-- A PostgreSQL `SEQUENCE` satisfies the third and violates the second: `nextval` is not
-- transactional, so a create that rolls back leaves its number unused and the sequence
-- permanently ahead. A read-then-write in application code violates the third, because two
-- transactions that read the same value both write it plus one.
--
-- So the counter is a **column on the repository row**, allocated with an atomic
-- `UPDATE ... RETURNING` inside the creating transaction:
--
--     UPDATE repositories SET next_issue_number = next_issue_number + 1
--     WHERE id = $1 RETURNING next_issue_number - 1
--
-- That is the mechanism the data-model concurrency table names — "Counter increments that
-- must not lose updates | Atomic `UPDATE ... SET n = n + 1`, never read-modify-write" — and
-- it satisfies all three constraints at once. The `UPDATE` takes the row's lock, so a second
-- creator waits rather than reading a stale value; and because the increment is part of the
-- transaction, a rollback undoes it, so the number is handed out again rather than skipped.
-- The cost is that issue creation serializes per repository, which is the right granularity:
-- two repositories never contend, and a repository's issue creation rate is not a hot path.
--
-- # The state-change history is the audit ledger
--
-- W2.2's acceptance requires that "closed and reopened issues keep a state-change history",
-- and `docs/reference/events.md` already defines exactly the records for it:
--
--     | `issue.created` / `issue.closed` / `issue.reopened` / `issue.deleted` | An issue
--       changes state | user | issue | 2 |
--
-- The scope is `issue`, which `audit_events_scope_type_check` did not permit, so this
-- migration widens it. **No separate history table is created**, and that is deliberate: the
-- ledger W1.10a built is append-only, already records the actor and the moment, and has no
-- foreign key to the issue — so a history survives the issue's deletion, which a table with
-- `ON DELETE CASCADE` would not.
--
-- Note that the catalogue has **no `issue.edited`**, so an edit of a title or body is not
-- audited here. Inventing the type would put a spelling into the contract that phase 2's
-- event workstream (W2.15) has not agreed to; the gap is recorded in the roadmap instead.

-- The scope of an issue-scoped record. The constraint is replaced rather than dropped, so the
-- permitted set stays enumerated: a future scope is a migration, which is what keeps this
-- list the contract rather than a comment.
ALTER TABLE audit_events DROP CONSTRAINT audit_events_scope_type_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_scope_type_check
    CHECK (scope_type IN ('instance', 'account', 'repository', 'issue'));

-- The counter. It starts at 1, so the first issue is number 1, and it is `BIGINT` because the
-- data-model forbids a smaller integer for a counter.
ALTER TABLE repositories ADD COLUMN next_issue_number BIGINT NOT NULL DEFAULT 1;

ALTER TABLE repositories ADD CONSTRAINT repositories_next_issue_number_check
    CHECK (next_issue_number >= 1);

CREATE TABLE issues (
    id            UUID        PRIMARY KEY,
    repository_id UUID        NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    -- number is the shared issue and pull-request number, unique within the repository and
    -- never reused. It is assigned from the counter above, never by the client.
    number        BIGINT      NOT NULL,
    title         TEXT        NOT NULL,
    body          TEXT        NOT NULL DEFAULT '',
    -- author_id is nullable so an issue survives its author's account being deleted. The
    -- record keeps its history — the audit ledger names the actor — and the issue shows no
    -- author rather than disappearing.
    author_id     UUID        REFERENCES users (id) ON DELETE SET NULL,
    state         TEXT        NOT NULL DEFAULT 'open',
    -- closed_at and closed_by record the most recent close, so a reopened-then-closed issue
    -- reports the close that is in force. Every close and reopen is in the ledger.
    closed_at     TIMESTAMPTZ,
    closed_by     UUID        REFERENCES users (id) ON DELETE SET NULL,
    -- created_at is immutable: nothing updates it. updated_at is set by the database on every
    -- write, so the application never generates "now" (data-model typing rules).
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- version implements the optimistic concurrency the data-model requires for "two writers
    -- on one editable record": an edit names the version it read, and a mismatch is a visible
    -- conflict rather than a silent overwrite.
    version       INTEGER     NOT NULL DEFAULT 1,

    -- Text that reaches the UI is bounded by a CHECK, "so a hostile client cannot store a
    -- megabyte in a title" (data-model typing rules).
    CONSTRAINT issues_title_check CHECK (length(title) BETWEEN 1 AND 256),
    -- The body bound is generous for a long report and small enough that a single row cannot
    -- dominate a page: 64 KiB.
    CONSTRAINT issues_body_check CHECK (length(body) <= 65536),
    -- An enumeration is text with a named CHECK, "never a PostgreSQL enum type, so a new value
    -- is a migration rather than a type rewrite".
    CONSTRAINT issues_state_check CHECK (state IN ('open', 'closed')),
    -- The two closed columns move together: a closed issue names when, and an open one names
    -- neither. Without this, a reopen that cleared one and not the other would be storable.
    CONSTRAINT issues_closed_pairs_check CHECK (
        (state = 'closed') = (closed_at IS NOT NULL) AND
        (state = 'closed') = (closed_by IS NOT NULL)
    ),
    CONSTRAINT issues_version_check CHECK (version >= 1),

    -- The number is unique per repository, which is also what makes `/{owner}/{repo}/issues/{number}`
    -- a lookup by index rather than a scan.
    CONSTRAINT issues_repository_id_number_key UNIQUE (repository_id, number)
);

-- The list query: one repository's issues filtered by state, newest first, keyset-paginated on
-- (created_at, id) because `data-model.md` makes keyset pagination the default for a
-- continuously growing collection.
--
-- The index carries `id` last so the keyset predicate `(created_at, id) < ($1, $2)` is
-- satisfied by the index alone, and `state` before `created_at` because every list the
-- surface offers filters by it — the default view is open issues.
CREATE INDEX issues_repository_id_state_created_at_idx
    ON issues (repository_id, state, created_at DESC, id DESC);

-- The author index serves two things: the "issues I opened" filter a later workstream adds,
-- and the `ON DELETE SET NULL` path. Without it, deleting an account scans the whole table to
-- find the rows it must detach, which is the named query that makes this index earn its place
-- rather than being speculative.
CREATE INDEX issues_author_id_idx ON issues (author_id);

-- closed_by needs the same treatment for the same reason: it is a foreign key, so a user
-- deletion has to find the rows that reference it.
CREATE INDEX issues_closed_by_idx ON issues (closed_by);

-- # Retention
--
-- An issue row is bounded by the repository that owns it: deleting the repository cascades to
-- its issues, which is why `repository_id` carries `ON DELETE CASCADE`. There is no
-- time-based retention rule to state, because the volume is bounded by how many issues a
-- repository accumulates rather than by time — unlike `audit_events`, whose retention rule is
-- recorded in 0005.
--
-- Deleting an issue is a **hard delete**, and the audit ledger is what makes it recoverable in
-- the sense that matters: `issue.deleted` records who deleted it and when, and the ledger has
-- no foreign key to this table, so the record outlives the row. A soft delete would instead
-- put a predicate on every query in this file and every query a later workstream writes.
