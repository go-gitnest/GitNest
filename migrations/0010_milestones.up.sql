-- Milestones, and an issue's milestone.
--
-- # The numbering is a sequence, not a rank
--
-- `urls-and-identifiers.md` §6.5 and L380 require a milestone to be "numbered per repository in its own
-- sequence, never reused", with "the same contiguity and no-reuse rules" as issues. That is the same
-- requirement W2.2a's issue numbering answers, so it takes the same mechanism: a counter **column on the
-- repository row**, allocated with an atomic `UPDATE ... RETURNING` inside the creating transaction.
--
-- **It is deliberately not derived the way a comment's number is.** A comment's number is its rank in
-- `(created_at, id)`, which is stable because a comment is never hard-deleted — but a milestone **can**
-- be deleted, and the rule says the number is "never reused" even then. A rank would renumber every
-- later milestone the moment one was removed, which is exactly what the rule forbids. So this is a
-- counter, and the difference is the requirement's, not a preference.
--
-- # Why two counters rather than one
--
-- The issue counter and the milestone counter are separate columns on `repositories`, because §6.5 says
-- the milestone sequence is its own. A single shared counter would make `/issues/3` and `/milestone/3`
-- mean different things from what their own sequences would produce, and a repository that created many
-- milestones would push its issue numbers forward for no reason.
--
-- # An issue has at most one milestone
--
-- The attachment is a **nullable column on `issues`**, not a join table like `issue_labels`. An issue may
-- carry many labels but only one milestone, so the cardinality differs and the model has to as well: a
-- column makes "one milestone" an expressible constraint rather than something the code has to remember,
-- and it makes the progress count a plain join rather than a distinct-count over an attachment table.
-- `ON DELETE SET NULL` is the right action — deleting a milestone detaches its issues and does not delete
-- them, mirroring the label decision.
--
-- # The due date
--
-- The payload catalogue names the field `due_on`, which is GitHub's, and a milestone due date is a
-- **calendar date** rather than an instant. `data-model.md` nevertheless requires that "no naive
-- timestamps" be stored and that "every temporal column is `timestamptz`", so the column is `timestamptz`
-- holding the date at **midnight UTC** — which is also exactly what GitHub returns for `due_on`
-- (`2026-10-14T00:00:00Z`). The alternative, a `date` column, would be a temporal column outside the
-- rule, and the application must not invent a zone for a value that has none.

ALTER TABLE audit_events DROP CONSTRAINT audit_events_scope_type_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_scope_type_check
    CHECK (scope_type IN ('instance', 'account', 'repository', 'issue', 'comment', 'label', 'milestone'));

-- The milestone counter. It starts at 1 and is separate from `next_issue_number`, because §6.5 gives
-- milestones their own sequence.
ALTER TABLE repositories ADD COLUMN next_milestone_number BIGINT NOT NULL DEFAULT 1;

ALTER TABLE repositories ADD CONSTRAINT repositories_next_milestone_number_check
    CHECK (next_milestone_number >= 1);

CREATE TABLE milestones (
    id            UUID        PRIMARY KEY,
    repository_id UUID        NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    -- number is the per-repository milestone number, never reused, and what the route addresses.
    number        BIGINT      NOT NULL,
    title         TEXT        NOT NULL,
    -- title_folded is the comparison form, computed by the application with ASCII-only folding. It is
    -- never displayed: it is a lookup key, and leaving it out of every projection keeps a scan and its
    -- struct in step.
    title_folded  TEXT        NOT NULL,
    -- description is optional, so an empty string is a legitimate value rather than a missing one.
    description   TEXT        NOT NULL DEFAULT '',
    -- due_on is the calendar date at midnight UTC. It is nullable, because a milestone need not have one.
    due_on        TIMESTAMPTZ,
    state         TEXT        NOT NULL DEFAULT 'open',
    closed_at     TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- §6.5: a title is "1–255 characters", with case-folded uniqueness "across open and closed
    -- milestones". The service trims before validating, so the CHECK that the trimmed value is unchanged
    -- makes the rule a property of the table rather than of one code path.
    CONSTRAINT milestones_title_check CHECK (
        length(title) BETWEEN 1 AND 255 AND title = btrim(title)
    ),
    -- **No length is fixed for a milestone description in the documents.** `data-model.md` requires a
    -- bound for text that reaches the UI but §6.5 bounds the title only, so the value is chosen rather
    -- than derived: 65,536 characters, matching an issue body, because a milestone description is a
    -- comparable piece of prose and being more restrictive would be arbitrary.
    CONSTRAINT milestones_description_check CHECK (length(description) <= 65536),
    CONSTRAINT milestones_state_check CHECK (state IN ('open', 'closed')),
    -- The two closed columns move together, exactly as an issue's do: a closed milestone names when, and
    -- an open one names neither.
    CONSTRAINT milestones_closed_pairs_check CHECK (
        (state = 'closed') = (closed_at IS NOT NULL)
    ),

    CONSTRAINT milestones_repository_id_number_key UNIQUE (repository_id, number),
    -- **The case-folded uniqueness is on a stored folded column**, not on `lower(title)`: §6.1 fixes the
    -- comparison as "Unicode-free ASCII case folding", which is what `identifier.Fold` produces and what
    -- a collation-dependent `lower()` would not.
    --
    -- The uniqueness is **across open and closed milestones**, which is what §6.5 says: closing a
    -- milestone does not free its title for a second one, so this constraint has no state predicate.
    CONSTRAINT milestones_repository_id_title_folded_key UNIQUE (repository_id, title_folded)
);

-- The list query: one repository's milestones, newest first. The unique constraint's index covers
-- `(repository_id, number)`, which is the route lookup; this serves the ordered list.
CREATE INDEX milestones_repository_id_created_at_idx
    ON milestones (repository_id, created_at DESC, id DESC);

-- An issue's milestone. `ON DELETE SET NULL` detaches the issues rather than deleting them, which is the
-- same decision the label attachment makes and what stops a milestone deletion from being destructive.
ALTER TABLE issues ADD COLUMN milestone_id UUID REFERENCES milestones (id) ON DELETE SET NULL;

-- The progress count and the issue list's milestone filter. A partial index would not help: an issue
-- without a milestone is a legitimate filter value (`milestone=none`), so the index has to cover nulls.
CREATE INDEX issues_milestone_id_idx ON issues (milestone_id);

-- # Retention
--
-- A milestone and its attachments are bounded by the repository: deleting a repository cascades to its
-- milestones, and each milestone's `issues.milestone_id` is set to null by that deletion. There is no
-- time-based window, because the volume is bounded by how many milestones a repository defines —
-- unlike `audit_events`, whose rule 0005 records.
--
-- # Why this migration is reversible by the previous release
--
-- It adds a column to `repositories`, a column to `issues`, and one table. The previous release's code
-- writes neither new column and does not read the table, so it is unaffected; a rollback drops them and
-- loses the milestones and the attachments, which is the limit worth stating because it is the one thing
-- here that is not recoverable.
