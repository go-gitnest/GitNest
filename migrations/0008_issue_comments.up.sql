-- Issue comments and their edit history.
--
-- # Why a revision table exists, and why it is not an audit event
--
-- W2.3 requires "an append-only revision record for edits", and its acceptance requires that
-- "editing a comment records the previous body and does not rewrite history visible in the UI".
--
-- The audit catalogue (`docs/reference/events.md` §4.12) defines exactly **one** comment action:
--
--     | `comment.deleted` | A comment is deleted by its author or a moderator | user / moderator | comment | 2 |
--
-- There is no `comment.created` and no `comment.edited`. That is not an omission to work around,
-- because the two records answer different questions and the catalogue only governs one of them:
--
--   - **The audit ledger** is the security record: who acted, on what, when, from where. It has no
--     foreign key to the content it describes, which is deliberately what makes it outlive that
--     content. An `audit_events` row is not where a reader looks for a comment's previous text.
--   - **A revision** is domain data: the previous body, attributed and ordered, which the UI shows
--     as "edited". It belongs to the comment and goes when the comment goes, so it is a real
--     foreign key with a real cascade.
--
-- So an edit appends a revision and writes no audit record, and a **deletion writes the one comment
-- record the catalogue defines** and removes the history. This also resolves the contradiction
-- recorded against the issue parity line `Edit issue title and body with an append-only revision
-- record`: the "revision record" that line names is a table like this one, not a catalogue action.
-- Phase 3 applies the same mechanism to issues.
--
-- # Why deletion removes the history
--
-- A deleted comment's text must be gone, and a revision table that keeps it would make "deleted"
-- mean "hidden from the page" while the words sat one query away. So deleting a comment clears its
-- body **and** its revisions, inside one transaction — and because that is the consequential act,
-- it is the one that is audited.
--
-- # Ordering
--
-- W2.3 requires that "comment ordering is stable by creation time plus ID", and the acceptance
-- repeats it. The identifier breaks a tie on the same instant, which is what makes the order
-- **total**: two comments written in the same microsecond still have one defined order, so a page
-- cannot repeat or drop a row at a boundary. The `(created_at, id)` pair is therefore what both the
-- index and every query use, and it is the same keyset the issue list paginates by.

-- The ledger's scope list gains `comment`, so `comment.deleted` can be recorded. The constraint is
-- replaced rather than dropped, so the permitted set stays enumerated: a future scope is a
-- migration, which is what keeps this list the contract rather than a comment.
ALTER TABLE audit_events DROP CONSTRAINT audit_events_scope_type_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_scope_type_check
    CHECK (scope_type IN ('instance', 'account', 'repository', 'issue', 'comment'));

CREATE TABLE issue_comments (
    id         UUID        PRIMARY KEY,
    -- The comment belongs to one issue. `ON DELETE CASCADE` is what the acceptance's "never leaves a
    -- dangling timeline entry" requires at the storage level: a comment cannot survive the issue it
    -- was written on.
    issue_id   UUID        NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    -- Nullable so a comment survives its author's account being deleted, on the same reasoning the
    -- issue's author column records: the record keeps its history and the comment shows no author
    -- rather than disappearing.
    author_id  UUID        REFERENCES users (id) ON DELETE SET NULL,
    body       TEXT        NOT NULL,
    -- Deletion is **soft**: the row survives so the position in the conversation is not a gap, and
    -- its body is cleared so the words are actually gone.
    deleted_at TIMESTAMPTZ,
    deleted_by UUID        REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- edited_at is set when an edit is applied and cleared by nothing: a comment that was edited once
    -- reports that it was, even after a later edit.
    edited_at  TIMESTAMPTZ,
    -- The optimistic-concurrency guard, exactly as `issues.version` is: an edit names the version it
    -- read and a mismatch is a conflict rather than a silent overwrite.
    version    INTEGER     NOT NULL DEFAULT 1,

    -- The body is bounded by a CHECK, as every text column that reaches the UI is, and the bound
    -- matches the issue body's so a reader is not told two different limits for one kind of text.
    CONSTRAINT issue_comments_body_check CHECK (length(body) <= 65536),
    -- The three deletion columns move together, and a deleted comment carries **no body**. Enforcing
    -- that here rather than only in the delete statement means a future code path cannot leave the
    -- text behind while claiming the comment is deleted.
    CONSTRAINT issue_comments_deleted_check CHECK (
        (deleted_at IS NULL) = (deleted_by IS NULL) AND
        (deleted_at IS NULL OR length(body) = 0)
    ),
    CONSTRAINT issue_comments_version_check CHECK (version >= 1)
);

-- The list query: one issue's comments in creation order, keyset-paginated.
--
-- `id` is last so the keyset predicate `(created_at, id) > ($1, $2)` is satisfied by the index
-- alone, and because the order is total no row can be repeated or skipped at a page boundary.
CREATE INDEX issue_comments_issue_id_created_at_idx
    ON issue_comments (issue_id, created_at, id);

-- The author index serves the "comments I wrote" filter a later workstream adds and the
-- `ON DELETE SET NULL` path, which without it would scan the whole table to find the rows it must
-- detach. It is a named query rather than a speculative index.
CREATE INDEX issue_comments_author_id_idx ON issue_comments (author_id);

-- `deleted_by` is a foreign key, so a user deletion has to find the rows that reference it.
CREATE INDEX issue_comments_deleted_by_idx ON issue_comments (deleted_by);

CREATE TABLE comment_revisions (
    id         UUID        PRIMARY KEY,
    -- One comment's history. `ON DELETE CASCADE` matters twice over: deleting the comment removes its
    -- history, and deleting the **issue** cascades to the comment and from there to this table, so no
    -- revision can outlive the conversation it belongs to.
    comment_id UUID        NOT NULL REFERENCES issue_comments (id) ON DELETE CASCADE,
    -- revision counts from 1 within a comment, so a reader can see how many times it changed and the
    -- order is stated rather than inferred from timestamps.
    revision   INTEGER     NOT NULL,
    -- body is the body **as it was before that edit**, which is what "records the previous body"
    -- means: revision 1 holds the original text, revision 2 the text after the first edit.
    body       TEXT        NOT NULL,
    -- The version the comment was at when this revision was written, so the history can be tied to
    -- the concurrency guard's numbering rather than to a second scheme.
    version    INTEGER     NOT NULL,
    editor_id  UUID        REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT comment_revisions_revision_check CHECK (revision >= 1),
    CONSTRAINT comment_revisions_version_check CHECK (version >= 1),
    -- A body of any length up to the comment's own bound: the previous text was once a valid body.
    CONSTRAINT comment_revisions_body_check CHECK (length(body) <= 65536),
    -- Numbering is unique within a comment, which is what makes the history a sequence rather than
    -- a bag of snapshots.
    CONSTRAINT comment_revisions_comment_id_revision_key UNIQUE (comment_id, revision)
);

-- # Retention
--
-- A comment's history is bounded by the comment: the row count is the number of times somebody
-- edited it, and every revision goes when the comment or the issue goes. So there is no time-based
-- retention window to state, unlike `audit_events`, whose rule 0005 records. What bounds the table
-- growth is that a comment cannot be edited more times than it is edited, and a repository that
-- accumulates comments accumulates revisions in proportion — the same relationship the issue table
-- has to its repositories.
--
-- # Why this migration is reversible by the previous release
--
-- It only adds two tables. The previous release's code does not know them and cannot be affected by
-- them; a rollback drops them and loses the comment history, which is the limit worth stating
-- because it is the one thing here that is not recoverable.
