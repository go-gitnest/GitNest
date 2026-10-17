-- Repository archival, and the audit ledger the phase-1 security-relevant
-- operations write to.
--
-- Two changes in one migration because they belong to the same workstream: the
-- archive flag is one of the operations the ledger records, and shipping the
-- flag without the trail would leave a state change that nothing accounts for.
--
-- ## Why the audit table is here and not in phase 8
--
-- docs/architecture/data-model.md assigns `audit_events` to phases "1, 2, 8":
-- the table and its writes arrive with the first operations that need them, and
-- phase 8 adds the query surface, retention, export, and legal hold.
-- docs/reference/events.md marks `repo.visibility_changed`,
-- `repo.collaborator_added`, `repo.collaborator_role_changed`,
-- `repo.collaborator_removed`, `repo.archived`, `repo.unarchived`, and
-- `repo.default_branch_changed` as phase 1, and W1.10's own acceptance says
-- "visibility changes are audited". So the ledger is a phase-1 deliverable.
--
-- ## Retention plan
--
-- docs/architecture/data-model.md requires a table that grows without bound to
-- state its retention rule from the phase that introduces it. For this table:
--
--   * **Window:** no row is deleted in phase 1. The volume is a function of
--     administrative actions, not of traffic, so it grows slowly.
--   * **Who configures it:** the instance administrator, through the operator
--     console, which phase 8 W8.7 owns along with legal hold and export.
--   * **Partitioning:** none yet. The table is keyed by `created_at` with an
--     index named for the range query phase 8 will run, so converting to
--     monthly partitions later needs no column change.
--   * **Removal:** `DELETE` is reserved for the retention policy. `UPDATE` is
--     refused by trigger below, because an editable audit record is not a
--     record.
--
-- Rollback limit: additive. The previous release ignores `archived_at` and does
-- not read `audit_events`, so it still runs against this schema. Dropping the
-- table would lose the trail, which is why it is not reversible in the sense an
-- operator cares about.

ALTER TABLE repositories
    ADD COLUMN archived_at TIMESTAMPTZ;

-- A partial index for the only question phase 1 asks of the column: which
-- repositories are archived. It is partial because the answer is almost always
-- "none", and a full index would carry every row to say so.
CREATE INDEX repositories_archived_idx ON repositories (created_at DESC) WHERE archived_at IS NOT NULL;

-- The append-only ledger.
--
-- Column names follow docs/architecture/data-model.md. The record carries what
-- docs/roadmap/08-governance-and-enterprise.md says a record carries — actor,
-- principal kind, action, target, timestamp, source address, and user agent
-- where applicable — plus a structured detail blob.
CREATE TABLE audit_events (
    id             UUID PRIMARY KEY,
    -- The stable type from the catalogue in docs/reference/events.md:
    -- `<domain>.<object>_<past_tense_verb>`. It is never localized, never
    -- derived from a label, and never carries a name, slug, identifier, or
    -- count (events.md section 2.1).
    action         TEXT NOT NULL,
    -- The account that acted. Null for a system action and for an anonymous
    -- caller, which is why `actor_kind` carries the distinction.
    actor_id       UUID REFERENCES users(id) ON DELETE SET NULL,
    actor_kind     TEXT NOT NULL,
    -- What the record is about. Phase 1 scopes a record to a repository or to
    -- an account; organizations and enterprises join the check in phase 2 and 8.
    scope_type     TEXT NOT NULL,
    scope_id       UUID,
    -- The one thing that changed. events.md requires one record per change,
    -- each naming one subject.
    subject_type   TEXT NOT NULL,
    subject_id     UUID,
    detail         JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Where the caller was, when the request had a caller.
    source_address TEXT,
    user_agent     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT audit_events_action_check CHECK (action <> ''),
    CONSTRAINT audit_events_actor_kind_check CHECK (actor_kind IN ('user', 'admin', 'system', 'anonymous')),
    CONSTRAINT audit_events_scope_type_check CHECK (scope_type IN ('instance', 'account', 'repository')),
    -- An actor of kind `user` or `admin` is an account, so it must carry one;
    -- every other kind must not, so a record cannot claim an actor it lacks.
    CONSTRAINT audit_events_actor_pairs_check CHECK (
        (actor_kind IN ('user', 'admin')) = (actor_id IS NOT NULL)
    ),
    -- A scoped record names its scope; an instance-wide one does not.
    CONSTRAINT audit_events_scope_pairs_check CHECK (
        (scope_type = 'instance') = (scope_id IS NULL)
    )
);

-- Every foreign key has an index, because every foreign key is a lookup
-- direction (docs/architecture/data-model.md).
CREATE INDEX audit_events_actor_id_idx ON audit_events (actor_id);

-- Serves the range query phase 8's console runs, newest first. It is named here
-- rather than added then because a table that will be scanned by time should
-- carry the index from the start.
CREATE INDEX audit_events_created_at_idx ON audit_events (created_at DESC);

-- The scope index, for "what happened to this repository".
CREATE INDEX audit_events_scope_idx ON audit_events (scope_type, scope_id, created_at DESC);

-- The ledger is append-only.
--
-- `events.md` says audit records are "never editable through an API" and are
-- "removed only by retention or legal-hold policy". An API discipline would be a
-- convention; this is a guarantee, and it also stops a future migration or a
-- mistaken `UPDATE` from rewriting history. `DELETE` stays available because
-- retention and legal hold both need it.
CREATE FUNCTION audit_events_reject_update() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only: an existing record cannot be changed';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE ON audit_events
    FOR EACH ROW
    EXECUTE FUNCTION audit_events_reject_update();
