# Data model and migration rules

**Status:** Target design. Nothing in this document is implemented yet.
**Scope:** Naming and typing conventions across every table, the core entity map, concurrency and pagination rules, and the rules every migration must follow.

## Purpose

GitNest will accumulate well over a hundred migrations across ten phases, written by different workstreams. Without a fixed set of conventions, the schema drifts: the same concept gets three spellings, indexes appear without a query that needs them, and a migration that is unsafe on a live instance causes an outage. This document is the convention set that every workstream follows.

## Identifier and naming conventions

| Concern | Rule |
| --- | --- |
| Primary key | `id uuid primary key`, generated in Go as a time-ordered UUIDv7 so index inserts stay local |
| External identifiers | Names and slugs are separate columns from `id`. A URL never contains an internal identifier |
| Table names | Plural `snake_case`, for example `pull_requests` |
| Column names | Singular `snake_case`, for example `default_branch` |
| Foreign keys | `<singular entity>_id`, with an explicit `ON DELETE` action |
| Timestamps | `created_at`, `updated_at`, and `deleted_at` where soft deletion applies; always `timestamptz`, always UTC, defaulted by the server |
| Booleans | `is_<adjective>` or a `status` text column when more than two states are possible |
| Enumerations | `text` with a named `CHECK` constraint, never a PostgreSQL enum type, so a new value is a migration rather than a type rewrite |
| Unique constraints | `<table>_<columns>_key` |
| Indexes | `<table>_<columns>_idx`; partial indexes name their predicate in a comment |
| Check constraints | `<table>_<column>_check` |
| JSON | `jsonb` only for genuinely schemaless payloads (webhook payloads, workflow definitions, project field values), always with a Go type that validates it on write |

## Typing rules

- **No floating point for money, counters, quotas, or byte sizes.** Use `bigint` with a stated unit.
- **No naive timestamps.** Every temporal column is `timestamptz`; the application never generates "now" when the database can, except where ordering across nodes requires an application clock, which is documented at the call site.
- **Case-insensitive uniqueness** (usernames, e-mail addresses, repository names within an owner) is enforced by a unique index on a normalized column or on `lower(column)`, not by an application check that races.
- **Text length** is bounded by a `CHECK` for values that reach the UI, so a hostile client cannot store a megabyte in a title.
- **External content** (repository file contents, package blobs, artifacts, logs) is never stored in PostgreSQL; only metadata and references are.

## Identifier rules

- Internal identifiers are opaque UUIDs. A client that guesses one must still pass authorization, so identifiers are never treated as secrets.
- A user-facing identifier (username, organization, repository, team, label, milestone, package, environment, workspace) has a documented length, character set, case rule, uniqueness scope, and reserved-name list in [urls-and-identifiers.md](urls-and-identifiers.md).
- Issue and pull-request numbers share one per-repository sequence and are never reused, including after deletion.

## Index policy

- Every foreign key has an index, because every foreign key is a lookup direction we use.
- Every list endpoint's filter and sort order has a matching index, named in a comment next to the query it serves.
- No speculative index. An index without a named query is removed in review.
- Hot subsets (unresolved conversations, pending jobs, unrevoked sessions) use partial indexes.
- Uniqueness is enforced by a constraint, never by a read-then-write in application code.

## Pagination and ordering

- **Keyset pagination** is the default for large or continuously growing collections (commits, events, audit entries, jobs, package versions) using a stable, indexed sort key.
- **Offset pagination** is used only where the compatibility contract requires it (the GitHub-compatible REST surface), and those endpoints are documented as such.
- Every paginated query has a deterministic total order; ties are broken by `id`.

## Concurrency

| Situation | Mechanism |
| --- | --- |
| Two writers on one editable record | Optimistic concurrency with a version column or `updated_at` comparison; a conflict is a visible error, not a silent overwrite |
| A singleton operation (administrator bootstrap, instance setting seed) | Transaction-scoped advisory lock plus an existence check |
| A claim that must not be double-processed (invitation acceptance, job lease, release asset upload) | Conditional `UPDATE ... WHERE <state> IS NULL` returning the claimed row, inside the transaction that completes the work |
| `git ref` updates | Compare-and-swap through `git update-ref` with the expected old value, or a ref transaction |
| Counter increments that must not lose updates | Atomic `UPDATE ... SET n = n + 1`, never read-modify-write |

## Retention and volume

Tables that grow without bound need a retention rule from the phase that introduces them: audit events, job and run logs, webhook deliveries, notification records, session and token history, and workspace snapshots. The rule states the retention window, who configures it, and whether the table is partitioned by time. A table without a retention or partition plan is a defect to raise in review.

## Migration rules

These rules are not optional; they are what keeps an upgrade from taking an instance down.

1. **Forward only.** A migration is never edited after it may have been applied. A correction is a new migration. The ledger records a SHA-256 checksum of each applied file, and the runner refuses to run when a recorded file's contents have changed, so an edit is caught instead of being silently skipped.
2. **One logical change per file**, named `NNNN_short_description.up.sql` with a four-digit zero-padded sequence, so lexical order is application order for the first 9999 migrations and the ledger reads as a history. A stray `.sql` file in `migrations/` whose name does not match is a startup failure, not something to ignore, because a mistyped suffix is invisible on a case-insensitive filesystem.
3. **Every migration runs in its own transaction** and writes its ledger row in that same transaction, so a failure leaves neither schema work nor a ledger entry behind.
4. **A migration must be safe while the previous release's binary is still running** (expand and contract):
   - Add columns as nullable or with a default; never add a `NOT NULL` column without one.
   - Never rename or drop a column or table in the same release that stops using it; stop using it, ship, then remove it in a later release.
   - Never narrow a type or add a constraint that existing rows can violate without a backfill first.
5. **Long backfills are jobs, not migrations.** A migration that would rewrite a large table moves the data in bounded batches through the durable job system, and the tightening migration ships in a later release.
6. **Index creation on a live table uses the concurrent form**, which cannot run inside a transaction. Such a migration is explicitly marked with `-- gitnest:no-transaction` in its leading comment block, and the runner honours that marker: it executes the whole file as one statement and then records the ledger row in a **separate** statement. Because those two steps are not atomic, such a migration must be written to be safe to re-run (`IF NOT EXISTS` and equivalent guards), and it states that requirement in its own comment.
7. **No data loss without an audit event.** A migration that deletes or transforms user-visible data records what it did.
8. **Every migration states its rollback limit** in a comment at the top: whether the previous release can still run against the new schema (it must, per rule 4) and what is not reversible.
9. **Destructive migrations are separated from additive ones** so an operator can stop between them.

## Core entity map

Groups, with the phase that introduces them:

| Group | Entities | Phase |
| --- | --- | --- |
| Identity | `users`, `sessions`, `invitations`, `personal_access_tokens`, `ssh_keys`, `gpg_keys`, `two_factor_credentials`, `recovery_codes` | 1, 4, 8 |
| Organization | `organizations`, `enterprises`, `teams`, `team_members`, `organization_members`, `outside_collaborators` | 2, 8 |
| Repository | `repositories`, `repository_collaborators`, `branch_protections`, `rulesets`, `deploy_keys`, `topics`, `stars`, `watchers` | 1, 2, 7, 8 |
| Collaboration | `issues`, `issue_comments`, `labels`, `milestones`, `assignments`, `reactions`, `timeline_events`, `pull_requests`, `reviews`, `review_threads`, `review_comments` | 2, 3 |
| Planning | `projects`, `project_fields`, `project_items`, `discussions`, `discussion_categories`, `discussion_answers` | 7 |
| Delivery | `releases`, `release_assets`, `packages`, `package_versions`, `package_blobs` | 6 |
| Automation | `workflows`, `workflow_runs`, `workflow_jobs`, `job_steps`, `job_logs`, `artifacts`, `cache_entries`, `environments`, `secrets`, `variables`, `runner_registrations`, `check_runs` | 5 |
| Integration | `oauth_apps`, `apps`, `installations`, `installation_tokens`, `webhooks`, `webhook_deliveries` | 4 |
| Discovery | `search_documents` (index projection), `wiki_pages`, `pages_sites`, `pages_builds`, `user_follows` | 7 |
| Notification and audit | `notifications`, `notification_threads`, `subscriptions`, `email_deliveries`, `audit_events` | 1, 2, 8 |
| Cloud development | `workspaces`, `workspace_images`, `workspace_volumes`, `workspace_snapshots`, `workspace_ports` | 9 |
| Platform | `jobs`, `job_attempts`, `instance_settings`, `schema_migrations` | 0, 1 |

Every relation uses these identifiers. A table that stores a reference to user-facing content stores the internal identifier, never the name.

## Testing obligations

- Convention checks are part of review; a schema test asserts that every foreign key has an index and every table with a growing collection declares a retention or partition plan.
- Migration tests: applying to an empty database, re-running as a no-op, a deliberately failing migration rolling back both its schema work and its ledger row, and a migration marked as running outside a transaction being handled correctly.
- Concurrency tests for each claim pattern (invitation acceptance, job lease, ref update) proving that a parallel second attempt fails rather than duplicating work.
- A compatibility test that runs the previous release's queries against the new schema for the migrations that claim expand-and-contract safety.

## Open questions

- Whether time-ordered identifiers use UUIDv7 or ULID. UUIDv7 is preferred because PostgreSQL stores it natively as `uuid`; the decision is confirmed in the first workstream that creates a table.
