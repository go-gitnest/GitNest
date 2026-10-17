-- Account status, browser sessions, and single-use invitations.
--
-- Session and invitation tokens are 32 random bytes, so they are stored as
-- SHA-256 digests rather than password hashes: there is nothing to brute force,
-- and lookup by digest must stay fast (docs/architecture/secrets.md). The raw
-- value exists only in the response that issues it.
--
-- Rollback limit: additive. The previous release reads users without the new
-- columns and ignores the two new tables, so it still runs against this schema.
-- Dropping the tables is safe only while no session or invitation exists.

ALTER TABLE users
    ADD COLUMN status TEXT NOT NULL DEFAULT 'active',
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

ALTER TABLE users
    ADD CONSTRAINT users_status_check CHECK (status IN ('active', 'disabled')),
    -- The grammar allows at most 39 characters; the constraint keeps a row
    -- written by anything other than the service from exceeding it.
    ADD CONSTRAINT users_username_length_check CHECK (char_length(username) BETWEEN 1 AND 39);

-- A browser session. The CSRF token is bound to the session and stored as its
-- own digest, so a state-changing request can be rejected before it reaches a
-- handler (docs/security/threat-model.md, T-K4).
CREATE TABLE sessions (
    id              UUID PRIMARY KEY,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      BYTEA NOT NULL,
    csrf_token_hash BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,

    CONSTRAINT sessions_token_hash_key UNIQUE (token_hash)
);

-- Resolution is by digest, and revocation and expiry are both part of the
-- predicate, so the partial indexes cover the only lookup the request path
-- performs. The user index serves "revoke every session of this account".
CREATE INDEX sessions_user_id_idx ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_live_expiry_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

-- A single-use invitation. The address is stored folded, and it is the address
-- the account is created with: the invitation decides the email, never the
-- request that accepts it.
CREATE TABLE invitations (
    id          UUID PRIMARY KEY,
    email       TEXT NOT NULL,
    token_hash  BYTEA NOT NULL,
    created_by  UUID NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    accepted_by UUID REFERENCES users(id),
    revoked_at  TIMESTAMPTZ,

    CONSTRAINT invitations_token_hash_key UNIQUE (token_hash),
    -- An acceptance records who accepted it and when, or neither. The pair is
    -- written in one statement, so this cannot be satisfied halfway.
    CONSTRAINT invitations_accepted_pair_check
        CHECK ((accepted_at IS NULL) = (accepted_by IS NULL))
);

-- Deliberately no CHECK tying expires_at to created_at on either table: an
-- administrator shortening a session's or an invitation's lifetime is a
-- legitimate operation, and a constraint would forbid exactly that. The
-- service sets an expiry in the future when it creates the row.

CREATE INDEX invitations_email_idx ON invitations (lower(email));
