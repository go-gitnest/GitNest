# Secrets and credential storage

**Status:** Target design. Nothing in this document is implemented yet.
**Scope:** How GitNest stores passwords, tokens, keys, and encryption keys, and how secret material is kept out of logs, responses, backups, and job output.

## Purpose

GitNest holds many kinds of credential: user passwords, session and CSRF tokens, invitation and personal access tokens, OAuth client secrets, GitHub App private keys, webhook signing secrets, CI secrets and variables, SMTP and object-storage credentials, SAML signing material, and workspace secrets. Each needs the right storage primitive. Getting this wrong is not a bug that a later phase can fix cheaply, because it decides what a database leak exposes.

## The decision rule

| Value kind | Storage | Why |
| --- | --- | --- |
| Low-entropy human secret (a password) | Argon2id hash, parameters recorded in the value | Must stay expensive to crack after a leak |
| High-entropy random token (session, CSRF, invitation, personal access token, runner registration token, recovery code) | SHA-256 digest | There is nothing to brute force, and lookup by digest must stay fast |
| Public key material (SSH public keys, GPG public keys, deploy keys) | Plaintext | It is public by definition |
| Third-party secret the server must replay (OAuth client secret, App private key, webhook signing secret, SMTP password, object-storage secret, SAML key, CI secret value, workspace secret) | Authenticated encryption at rest | The server needs the plaintext, so hashing is impossible; leaking it must not hand over the value |

Two rules follow from the table:

- **A value that the server only ever compares is hashed, never encrypted.** Comparing a digest needs no key and cannot be reversed.
- **A value that the server must replay is encrypted, never stored in plaintext**, including in a table that looks internal.

## Password hashing

- **Algorithm:** Argon2id, PHC string encoding, `$argon2id$v=19$m=<memory>,t=<iterations>,p=<parallelism>$<salt>$<key>`.
- **Parameters:** `m=65536` (64 MiB), `t=3`, `p=2`, 16-byte salt, 32-byte key. They are recorded in every hash, so they can be raised later without invalidating existing credentials.
- **Verification:** constant time, and the parsed parameters are bounded (memory, iterations, parallelism) so a corrupted or hostile stored hash cannot request unbounded CPU or memory.
- **A password change re-hashes with current parameters.** Re-hashing every stored password on a parameter change is a background job, not a login-time surprise.
- **Longest accepted input is bounded** (1024 bytes) so one request cannot force unbounded hashing work.
- **Shortest accepted input is bounded too: 8 characters**, matching GitHub's documented minimum so a password accepted by one is accepted by the other. Composition is not judged — no required symbol classes — because such rules push users toward predictable patterns and a longer bound would be a product decision this document does not make. Validation runs before hashing, so a rejected password never costs a full Argon2id computation.
- Passwords are never stored, logged, echoed, or returned, and never accepted as a command-line argument. A command that mistakes an argument for a password reports the usage without echoing the value, because the terminal and anything capturing its output would otherwise record it.

## Random token generation and storage

- 32 bytes from a cryptographic random source, encoded with URL-safe base64 for transport.
- The raw value is returned exactly once, to the client that requested it. The database stores only the SHA-256 digest, and lookups are by digest.
- Token digests are compared in constant time where the value is not looked up by index (for example a CSRF token accompanying an already-resolved session).
- Every token class carries: purpose, owning account, creation time, expiry time, and where applicable a revoked-at time. Expiry is enforced in the query that resolves the token, not only in application code.

## Encryption of replayable secrets

- **Cipher:** an AEAD — AES-256-GCM where hardware acceleration is available, XChaCha20-Poly1305 where a 24-byte nonce is preferred. One is chosen and used consistently; the algorithm is recorded in the ciphertext envelope.
- **Envelope:** `v1:<key-id>:<base64 nonce>:<base64 ciphertext>`. The version and key identifier make rotation possible without a schema change.
- **Additional authenticated data:** the record identity (table, row identifier, and field name) is bound as AAD, so a ciphertext copied into another row fails to decrypt instead of silently decrypting to the wrong context.
- **Nonces:** random per encryption. A nonce is never reused with the same key.
- **Scope:** each encrypted field is encrypted independently, so a partial read does not expose unrelated values.

## Encryption keys

- **Master key:** `GITNEST_SECRET_KEY`, supplied through the environment or a file readable only by the service account. It is present at startup or the process refuses to start. It is never written to the database, logged, or returned by an endpoint.
- **Derivation:** per-purpose subkeys are derived from the master key with HKDF using a purpose label, so one purpose's compromise or rotation does not require touching the others.
- **Key identifier:** the envelope records which key identifier produced the ciphertext, so a rotation can decrypt with the old key and re-encrypt with the new one.
- **Rotation:** a documented procedure that adds a new key identifier, marks it current for writes, and re-encrypts existing values with a background job. Rotation must be resumable and idempotent.
- **Loss:** if the master key is lost, encrypted values are unrecoverable; hashed passwords are not. The documentation states this plainly, and the key is part of the backup set — stored **separately** from the database backup, because a backup that contains both the ciphertext and the key protects nothing.
- **Development and tests:** tests generate an ephemeral key per run. No key is committed to the repository.

## Secret lifecycle

- Every secret has an owner, a creation time, and either an expiry or an explicit "no expiry" that the operator chose knowingly. Rotation is possible without downtime: create the new value, deploy it, then revoke the old one.
- Revocation is immediate and server-side: a revoked session, token, or key fails on the next request, and any cache that could answer otherwise is invalidated on revocation.
- Installations and apps hold short-lived tokens scoped to installation permissions rather than long-lived credentials.

## Keeping secrets out of outputs

A single redaction boundary applies to every outbound channel:

- **Logs:** a registered-secret registry records the exact values (and their base64 and JSON-escaped forms) that must never appear. Every log write passes through it. Structured logging is mandatory so redaction happens on values, not on a rendered string.
- **Job and workspace output:** the same registry masks log chunks before they are stored or streamed, tested against multi-line, base64, and JSON-escaped values.
- **HTTP responses and errors:** no endpoint returns a stored credential; error bodies never include a DSN, a token, a key, or a repository path.
- **Metrics and traces:** labels never carry an account name, repository name, token, or file content. Cardinality is bounded deliberately, which also keeps secrets out.
- **Webhook payloads:** a payload never includes a secret value; signatures are computed from a secret and sent as a header, never in the body.
- **Backups:** encrypted values are present, the master key is not.

## Testing obligations

- Argon2id: PHC round trip, parameter bounds rejection, salted (two hashes of the same password differ), constant-time verification path.
- Tokens: digest-only storage, lookup by digest, expiry and revocation rejection, constant-time comparison where applicable.
- Encryption: round trip, AAD binding (a ciphertext moved to another row fails), envelope versioning, rotation with old and new key identifiers present.
- Redaction: a registered secret never appears in a log line, a job log chunk, an error body, a metric label, or a webhook payload, including multi-line and encoded forms.
- Startup: a missing or malformed master key fails startup with a message that does not include the key.
