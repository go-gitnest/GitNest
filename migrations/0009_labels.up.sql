-- Labels, and their attachment to issues.
--
-- # Case-folded uniqueness, and why the folded name is a column
--
-- `urls-and-identifiers.md` §6.5 requires a label name to have "case-folded uniqueness per
-- repository", and §6.1 fixes what folding means: "Owner names, team slugs, repository names, label
-- names, milestone titles, environment names, and package names are compared after **Unicode-free
-- ASCII case folding**. Display preserves the case chosen at creation."
--
-- So the comparison form is a **stored column**, folded by `identifier.Fold` — the helper the
-- repository and account names already use — and the uniqueness constraint is on it. That is the
-- data-model rule "Case-insensitive uniqueness ... is enforced by a unique index on a normalized
-- column or on `lower(column)`, not by an application check that races".
--
-- `lower()` would be wrong here for the same reason the rule names: it is Unicode- and
-- collation-dependent, so the index, a Go comparison, and the database would not necessarily agree.
-- A stored ASCII-folded column makes all three agree by construction. And because ASCII folding
-- leaves non-ASCII alone, `Ünïcödé` and `ünïcödé` are **two** labels rather than a collision — which
-- is what "Unicode-free" means, rather than an oversight.
--
-- # The colour is validated against a grammar, at the database too
--
-- `docs/security/threat-model.md` T-B2 requires that "user-controlled colors and identifiers are
-- validated against a grammar, never interpolated raw", and §6.5 fixes the grammar: "Six hexadecimal
-- digits, no `#` in storage, rendered with `#`".
--
-- The CHECK below is that grammar, and it admits **lowercase only** — the service normalizes on write,
-- so the stored value is drawn from a sixteen-character alphabet. That is the narrowest form the rule
-- can take, and it is what makes the value safe to place in a `style` attribute or a class name on the
-- read surfaces: nothing outside `[0-9a-f]{6}` can be stored, so no second escaping rule has to be
-- right for the value to be inert.
--
-- # Deleting a label does not delete issues
--
-- W2.4's acceptance requires that "deleting a label removes its attachments without deleting issues",
-- and the join table's two foreign keys express exactly that: the attachment rows cascade away with
-- the label, and the issues they pointed at are untouched. The reverse cascade is also correct — an
-- issue's deletion removes its attachments — so neither direction can leave a row pointing at
-- something gone.
--
-- # The join table has no surrogate identifier
--
-- `data-model.md` gives the primary key rule as "`id uuid primary key`, generated in Go as a
-- time-ordered UUIDv7 so index inserts stay local". **This table is the deliberate exception**, and
-- it is the project's first join table. An attachment has no identity of its own: it is the fact that
-- one label is on one issue, so a surrogate key would be a value nothing reads and a second index over
-- the same pair. The composite primary key *is* the uniqueness constraint — one label cannot be
-- applied to one issue twice — and its index serves the only lookup direction this table has, which is
-- "the labels on this issue". A `PRIMARY KEY` on a join table is therefore the constraint and the
-- index in one, rather than a rule followed for its own sake.

-- The ledger's scope list gains `label`, so `label.created` and `label.deleted` can be recorded. The
-- constraint is replaced rather than dropped, so the permitted set stays enumerated.
ALTER TABLE audit_events DROP CONSTRAINT audit_events_scope_type_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_scope_type_check
    CHECK (scope_type IN ('instance', 'account', 'repository', 'issue', 'comment', 'label'));

CREATE TABLE labels (
    id            UUID        PRIMARY KEY,
    repository_id UUID        NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    -- name is what a reader sees, and keeps the case chosen at creation.
    name          TEXT        NOT NULL,
    -- name_folded is the comparison form, computed by the application with ASCII-only folding. It is
    -- never displayed: it is a lookup key, and leaving it out of every projection keeps a scan and its
    -- struct in step.
    name_folded   TEXT        NOT NULL,
    -- color is six lowercase hex digits with no `#`, per §6.5.
    color         TEXT        NOT NULL,
    -- description is optional, so an empty string is a legitimate value rather than a missing one.
    description   TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- §6.5: "1–50 characters, any printable characters except a leading or trailing space". The
    -- service trims before validating, so a stored name cannot begin or end with one; the CHECK that
    -- enforces it here is that the trimmed value is unchanged, which makes the rule a property of the
    -- table rather than of one code path.
    CONSTRAINT labels_name_check CHECK (
        length(name) BETWEEN 1 AND 50 AND name = btrim(name)
    ),
    CONSTRAINT labels_color_check CHECK (color ~ '^[0-9a-f]{6}$'),
    -- **No length is fixed for a label description in the documents.** `data-model.md` requires that
    -- "text length is bounded by a CHECK for values that reach the UI, so a hostile client cannot store
    -- a megabyte in a title", but §6.5 bounds the name and the colour only. The value is therefore
    -- chosen rather than derived: 100 characters, which is GitHub's limit, so a client migrating from
    -- it does not have to shorten a description that already fit.
    CONSTRAINT labels_description_check CHECK (length(description) <= 100),

    -- The uniqueness the acceptance names, on the folded form rather than the displayed one.
    CONSTRAINT labels_repository_id_name_folded_key UNIQUE (repository_id, name_folded)
);

-- The list query: one repository's labels, ordered by the folded name so the order is stable and
-- matches a case-insensitive reader's expectation. The unique constraint above already provides an
-- index on (repository_id, name_folded), so this table needs no second one for its list.

CREATE TABLE issue_labels (
    -- The attachment. Both directions cascade, so neither a deleted issue nor a deleted label can leave
    -- an attachment pointing at something gone.
    issue_id   UUID        NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    label_id   UUID        NOT NULL REFERENCES labels (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (issue_id, label_id)
);

-- The reverse lookup — which issues carry a label, which the per-label issue list and the label's
-- issue count both need. The primary key's index covers `issue_id` first, so this is the other
-- direction and it is used: `urls-and-identifiers.md` §2.4 lists `/labels/{label}` as a "per-label
-- issue list".
CREATE INDEX issue_labels_label_id_idx ON issue_labels (label_id);

-- # Retention
--
-- A label and its attachments are bounded by the repository that owns them: deleting a repository
-- cascades to its labels and through them to their attachments. There is no time-based window to
-- state, because the volume is bounded by how many labels a repository defines rather than by time —
-- unlike `audit_events`, whose rule 0005 records.
--
-- # Why this migration is reversible by the previous release
--
-- It adds two tables and widens one CHECK. The previous release's code does not read either table and
-- cannot be affected by them; a rollback drops them and loses the labels, which is the limit worth
-- stating because it is the one thing here that is not recoverable.
