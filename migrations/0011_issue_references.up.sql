-- Cross-references between issues, and the link table they are recorded in.
--
-- # What this table is, and what it deliberately is not
--
-- `urls-and-identifiers.md` §7 states the display rule this table exists to serve:
--
--     | Display | `#123` in rendered Markdown resolves within the current repository only;
--       `owner/repo#123` requires read access to the target at render time and renders as plain text
--       when access is absent |
--
-- So a reference is **two facts**: that a piece of text named another issue, and that the naming was
-- recorded so both sides can show it. The first is derived at write time by `internal/references`; the
-- second is this table.
--
-- **The target is addressed by `(repository, number)` rather than by an issue identifier**, and there are
-- three reasons, each of which rules out a foreign key to `issues`:
--
--   1. **A reference may point at an issue that does not exist yet.** GitHub's closing keywords and
--      cross-references are routinely written before the target is opened — "this is the same bug as
--      #12" where `#12` is filed an hour later. A foreign key would refuse the row, and the reference
--      would then be invisible forever rather than resolving when the target appears.
--   2. **The target may be a pull request**, which shares the per-repository number sequence (§7:
--      "a pull request consumes the shared number at creation, so `#123` is unambiguous across both")
--      but lives in a table phase 3 introduces. A foreign key to `issues` would be wrong for half the
--      rows this table is for.
--   3. **Numbers are never reused** — the same section's "Never reused" row — so
--      `(repository, number)` stably identifies exactly one target for the life of the instance. The
--      pair is therefore as good an identifier as the row's own key, without either of the problems
--      above.
--
-- # Why both a source issue and an optional source comment
--
-- A reference can appear in an issue's body or in a comment on it, and the timeline shows which. The
-- comment is nullable and the body is not, because every reference belongs to an issue either way: a
-- comment's reference is also the issue's, which is what makes "recorded in both timelines" true
-- without a second row.
--
-- # A deleted comment takes its references with it
--
-- Both source columns cascade. That is deliberate for **both** directions of the deletion: an issue's
-- deletion removing its own references is obviously right, and a comment's deletion removing the
-- references its text contained is right for a subtler reason — W2.3's delete clears the comment's body,
-- so the text that named the target no longer exists and a timeline entry saying it did would be a claim
-- about text nobody can read.
--
-- **The incoming side is deliberately not deleted by these cascades.** If issue A's text referenced
-- issue B and A is later deleted, B's timeline loses the "referenced by A" entry, because the row goes
-- with A. That is the intended behaviour: a cross-reference is evidence *of the text that made it*, and
-- once that text is gone the evidence is gone. Keeping it would leave B's timeline pointing at a
-- deleted issue.

CREATE TABLE issue_references (
    id                   UUID        PRIMARY KEY,
    -- The issue whose text carries the reference. Every reference has one, whether it came from the
    -- body or a comment, because that is what makes the outgoing timeline entry belong to an issue.
    source_issue_id      UUID        NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    -- Null when the reference is in the issue's own body, the comment's identifier otherwise.
    source_comment_id    UUID        REFERENCES issue_comments (id) ON DELETE CASCADE,
    -- The target, addressed as §7 addresses it.
    target_repository_id UUID        NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    target_number        BIGINT      NOT NULL,
    -- created_at is the instant the referencing text was written, not the instant this row was written.
    -- The default exists for a direct insert, but every write sets it from the source text's own
    -- timestamp so that a reference recorded by a later re-parse still belongs where it appeared in the
    -- conversation — which is what makes the timeline's order reflect the conversation rather than the
    -- order rows happened to be inserted.
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT issue_references_target_number_check CHECK (target_number >= 1),

    -- **One reference per (source, target) pair, not per occurrence.** A text that says `#5` three times
    -- is one relationship; recording it three times would make the timeline show the same fact three
    -- times and the "recorded in both timelines" link appear thrice.
    --
    -- `NULLS NOT DISTINCT` is load-bearing and PostgreSQL 15 or newer is therefore required: a unique
    -- constraint treats NULLs as distinct by default, so without it an issue body's references — whose
    -- `source_comment_id` is always NULL — would not collide at all and every re-save would insert
    -- duplicate rows.
    CONSTRAINT issue_references_source_target_key
        UNIQUE NULLS NOT DISTINCT (source_issue_id, source_comment_id, target_repository_id, target_number)
);

-- # A self-reference is refused by the service, not here, and the reason is structural
--
-- "An issue references itself" needs a comparison between the source's own `(repository, number)` and the
-- target pair, and **a `CHECK` cannot join** — the source is an identifier in this table while the target
-- is a pair, so the rule has no expression here. A trigger could do it, and a trigger is the wrong tool
-- for something the parser already knows: the service holds the source issue's repository and number, so
-- it skips a reference that resolves to the issue being written. That is recorded in
-- `internal/references` rather than enforced by a mechanism this table cannot state.

-- The **outgoing** direction: one issue's references, which is what its own timeline shows. The unique
-- constraint's index serves this too, since it leads with `source_issue_id`.
CREATE INDEX issue_references_source_idx ON issue_references (source_issue_id, created_at);

-- The **incoming** direction: "what referenced this issue", which is what the *target's* timeline shows.
-- This is a different direction from the constraint's index and a different query, which is why it is a
-- second index rather than a speculative one — `urls-and-identifiers.md`'s "every foreign key has an
-- index, because every foreign key is a lookup direction we use" applies to the target pair as well.
CREATE INDEX issue_references_target_idx
    ON issue_references (target_repository_id, target_number, created_at);

-- One comment's references, so re-parsing a comment before overwriting its rows is a lookup rather than
-- a scan. Comments are the bulk of referencing text, so this is the common path.
CREATE INDEX issue_references_comment_idx
    ON issue_references (source_comment_id) WHERE source_comment_id IS NOT NULL;

-- # Retention
--
-- A reference is bounded by the text that carries it: deleting the issue or the comment removes its
-- outgoing rows, and the incoming rows of the *target* are untouched. There is no time-based window,
-- because the volume is bounded by how many references a repository's text makes — unlike
-- `audit_events`, whose rule 0005 records.
--
-- # Why this migration is reversible by the previous release
--
-- It adds one table. The previous release's code does not read it and cannot be affected; a rollback
-- drops it and loses the recorded references, which is the one thing here that is not recoverable.
