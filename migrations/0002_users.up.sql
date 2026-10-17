-- Accounts, and with them the instance's first administrator.
--
-- The display name and its folded form are separate columns: comparison is
-- ASCII-case-insensitive while display preserves the case the account was
-- created with, and the unique constraint is on the folded form
-- (docs/architecture/urls-and-identifiers.md section 6.2). Usernames are ASCII
-- by validation, so lower() is the same ASCII folding the application applies.
--
-- Rollback limit: additive. No released binary read accounts before this
-- migration, so the previous release still runs against this schema. Dropping
-- the table is safe only while no account exists.

CREATE TABLE users (
    id              UUID PRIMARY KEY,
    username        TEXT NOT NULL,
    username_folded TEXT NOT NULL,
    email           TEXT NOT NULL,
    password_hash   TEXT NOT NULL,
    is_admin        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT users_username_folded_key UNIQUE (username_folded),
    CONSTRAINT users_email_key UNIQUE (email),
    -- The two forms cannot drift: whatever writes a row must fold the display
    -- name, so a lookup by folded name always finds it.
    CONSTRAINT users_username_folded_matches CHECK (username_folded = lower(username))
);

-- Deliberately no constraint limiting how many accounts carry is_admin:
-- later phases grant and revoke the role, so a one-row rule here would have to
-- be dropped again. "Exactly one initial administrator" is enforced by the
-- bootstrap transaction, which takes an advisory lock and then checks whether
-- an administrator already exists.
