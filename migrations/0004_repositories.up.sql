-- Repositories and their direct collaborators.
--
-- Column and constraint names follow docs/architecture/data-model.md: plural
-- snake_case tables, `id uuid primary key`, display name and folded name as
-- separate columns, text enumerations with a named CHECK, and
-- `<table>_<columns>_key` / `_idx` / `_check` naming.
--
-- `storage_id` is deliberately not the primary key. It addresses the bytes on
-- disk and is the one identifier that must survive a rename and a transfer, so
-- it is a separate immutable column. See the storage note at the end of this
-- file for the layout rule that phase 1's storage workstream owns.
--
-- Rollback limit: additive. The previous release does not read these tables, so
-- it still runs against this schema. Dropping them is safe only while no
-- repository exists.

CREATE TABLE repositories (
    id            UUID PRIMARY KEY,
    -- Phase 1 repositories belong to a user. Organizations arrive in phase 2,
    -- which adds the owner discriminator and rebuilds the list index quoted in
    -- docs/architecture/performance.md to its documented three-column form.
    owner_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    name_folded   TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    -- Two values in phase 1. `internal` (visible to an enterprise's members)
    -- arrives with enterprises in phase 8 as an added CHECK value, which is a
    -- migration rather than a type rewrite (docs/architecture/data-model.md).
    visibility    TEXT NOT NULL DEFAULT 'private',
    -- Null until the first push establishes a branch, matching the
    -- GitHub-compatible payload where an empty repository has no default branch.
    default_branch TEXT,
    storage_id    UUID NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Uniqueness is on the folded name, per owner, per
    -- docs/architecture/urls-and-identifiers.md section 6.3, so two different
    -- owners may each hold `api`.
    CONSTRAINT repositories_owner_name_folded_key UNIQUE (owner_id, name_folded),
    CONSTRAINT repositories_storage_id_key UNIQUE (storage_id),
    CONSTRAINT repositories_visibility_check CHECK (visibility IN ('private', 'public')),
    -- The two name forms cannot drift: whatever writes a row must fold the
    -- display name, so a lookup by folded name always finds it.
    CONSTRAINT repositories_name_folded_matches CHECK (name_folded = lower(name)),
    -- Lengths are bounded here as well as in the service, so a hostile client
    -- cannot store a megabyte in a name or a description through any path.
    CONSTRAINT repositories_name_length_check CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT repositories_description_length_check CHECK (char_length(description) <= 350)
);

-- Serves the owner's repository list, filtered by visibility and ordered by
-- recent activity (docs/architecture/performance.md, "Repository list by
-- owner, visibility, recent activity"). Visibility is in the index because the
-- list query filters on it before it paginates.
CREATE INDEX repositories_owner_visibility_updated_idx
    ON repositories (owner_id, visibility, updated_at DESC);
-- Serves the same list when it must also include repositories the viewer
-- reaches only as a collaborator, and the "which repositories can this account
-- read" direction.
CREATE INDEX repositories_visibility_updated_idx
    ON repositories (visibility, updated_at DESC);

-- A direct collaborator grant on one repository.
--
-- Only read, write, and admin are storable: ownership is not a grant, it is the
-- repository's owner_id, so a stored 'owner' role would be duplicated truth
-- (docs/glossary.md, "Role versus permission"). Permissions are derived from
-- these roles, visibility, and ownership by internal/repository; they are never
-- stored as their own rows.
CREATE TABLE repository_collaborators (
    repository_id UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role          TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (repository_id, user_id),
    CONSTRAINT repository_collaborators_role_check CHECK (role IN ('admin', 'write', 'read'))
);

-- Every foreign key has an index because every foreign key is a lookup
-- direction (docs/architecture/data-model.md). The primary key already covers
-- lookups by repository_id, so only the user direction is added.
CREATE INDEX repository_collaborators_user_id_idx ON repository_collaborators (user_id);
