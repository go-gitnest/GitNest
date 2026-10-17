-- Personal access tokens.
--
-- # Why this migration is in phase 1
--
-- The plan is inconsistent about when this arrives, and the inconsistency is resolved
-- towards phase 1 because the phase-1 **exit criteria** require the capability:
--
--   * `docs/roadmap/01-identity-and-repository-hosting.md` lists "Accounts and their
--     credentials, browser sessions and personal access tokens" in phase 1 scope.
--   * The same file's exit criteria require that "an invited contributor can sign in,
--     create a private repository, push a local branch, browse the result, and clone it
--     from a second working copy" — which cannot happen without a credential a Git
--     client can present.
--   * `docs/parity-checklist.md` marks "Personal access token creation showing the value
--     once" as **P1**.
--   * Against those: `docs/reference/events.md` marks `token.created` as phase 4, and
--     `docs/roadmap/04-api-and-extensibility.md` W4.8 owns the full model.
--
-- So this delivers **the minimum a pusher needs** — a classic token with the `repo`
-- scope, shown once, stored as a digest, revocable — and leaves W4.8 its fine-grained
-- tokens, its scope strings beyond `repo`, its resource selectors, its last-used
-- instant, and its expiry policy. That split is recorded in the roadmap rather than
-- decided here.
--
-- # Storage
--
-- `docs/architecture/secrets.md` puts a personal access token in the hashed category
-- with the session and invitation tokens: "High-entropy random token (session, CSRF,
-- invitation, personal access token, ...) → SHA-256 digest". Nothing about it is
-- encrypted, because the server only ever compares it.
--
-- The display prefix is stored in the clear, deliberately. It is the first characters of
-- the token, it exists so a human can tell two tokens apart in a list, and
-- `docs/architecture/administration.md` requires an administrator to be able to "revoke
-- a personal access token by its display prefix". A prefix short enough to be useful is
-- far too short to be a secret on its own — the digest is what authenticates.

CREATE TABLE personal_access_tokens (
    id             UUID        PRIMARY KEY,
    user_id        UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- name is what the token is for, as the owner wrote it.
    name           TEXT        NOT NULL,
    -- token_prefix is the leading characters, shown in a list and accepted by the admin
    -- revoke command. It is not a credential.
    token_prefix   TEXT        NOT NULL,
    -- token_hash is the SHA-256 digest of the whole token, and it is what a lookup uses.
    token_hash     BYTEA       NOT NULL,
    -- scopes is the granted scope set. Phase 1 grants exactly one value, `repo`, and the
    -- CHECK keeps an unrecognised scope out until W4.8 defines the rest: storing a scope
    -- the code does not enforce would be worse than refusing it, because it would look
    -- like a restriction that is not applied.
    scopes         TEXT[]      NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- last_used_at is NULL until a request presents the token. W4.8 owns updating it;
    -- the column exists now so that adding the update is not a migration.
    last_used_at   TIMESTAMPTZ,
    -- revoked_at is how a token stops working. Revocation is a timestamp rather than a
    -- delete, because the audit record has to name a token that still exists.
    revoked_at     TIMESTAMPTZ,

    CONSTRAINT personal_access_tokens_name_check
        CHECK (length(name) BETWEEN 1 AND 100),
    CONSTRAINT personal_access_tokens_prefix_check
        CHECK (length(token_prefix) BETWEEN 8 AND 16),
    CONSTRAINT personal_access_tokens_hash_check
        CHECK (octet_length(token_hash) = 32),
    CONSTRAINT personal_access_tokens_scopes_check
        CHECK (scopes <@ ARRAY['repo']::TEXT[] AND array_length(scopes, 1) >= 1),
    CONSTRAINT personal_access_tokens_revoked_after_creation_check
        CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

-- The lookup an authenticated request performs: one digest, one live row. It is unique
-- rather than a plain index because two rows for one digest would make the decision
-- ambiguous, and `performance.md` lists exactly this index for the token tables.
CREATE UNIQUE INDEX personal_access_tokens_token_hash_key
    ON personal_access_tokens (token_hash);

-- The listing an account's settings page performs, and what the admin revoke command
-- resolves a prefix against. Partial, because a revoked token is never a candidate for
-- either and the live set is the small one.
CREATE INDEX personal_access_tokens_live_user_idx
    ON personal_access_tokens (user_id, created_at DESC)
    WHERE revoked_at IS NULL;

-- Resolving a display prefix is an administrator action, so it scans the live set of one
-- account at most. It is a plain index rather than a unique one: two tokens may share a
-- prefix, and the admin command reports the ambiguity instead of guessing.
CREATE INDEX personal_access_tokens_live_prefix_idx
    ON personal_access_tokens (token_prefix)
    WHERE revoked_at IS NULL;
