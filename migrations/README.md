# Migrations

Ordered, forward-only SQL migrations. The runner that applies them lives in
`internal/platform/postgres` (`Client.Migrate`), and the rules they must follow
are in [../docs/architecture/data-model.md](../docs/architecture/data-model.md).

## Naming

```text
NNNN_short_description.up.sql
```

Four digits, zero-padded, so lexical order is application order up to 9999
migrations: `0001_instance_settings.up.sql`, `0002_repositories.up.sql`. Every
name is validated by the runner, and a name that does not match is a startup
failure rather than a silently ignored file.

## Rules

1. **Forward only.** A migration is never edited after it may have been applied.
   Every applied migration's digest is recorded in the `schema_migrations`
   ledger, and the runner refuses to run when a recorded migration's content has
   changed.
2. **One logical change per file.** A correction is a new file.
3. **Each migration and its ledger row share one transaction**, unless the file
   opts out (below).
4. **Safe while the previous release's binary is still running** (expand and
   contract): add nullable or defaulted columns, stop using a column before
   dropping it, backfill through a job rather than inside a migration.
5. **State the rollback limit** in a comment at the top of the file: what a
   rollback to the previous release still supports, and what is irreversible.
6. **Separate destructive migrations from additive ones** so an operator can
   stop between them.

## Running outside a transaction

`CREATE INDEX CONCURRENTLY` and a few other statements cannot run inside a
transaction. A migration may opt out by placing this marker in its leading
comment block, before the first statement:

```sql
-- gitnest:no-transaction
CREATE INDEX CONCURRENTLY ...;
INSERT INTO schema_migrations (name, checksum) VALUES (...);  -- not needed; the runner records it
```

The runner executes the whole file with a single statement, then records the
ledger row in a **separate** statement. Because those two steps are not atomic,
such a migration **must be written to be safe to re-run** (use
`IF NOT EXISTS` and equivalent guards): if the process stops between the DDL and
the ledger insert, the next run repeats the file.

## The ledger

`schema_migrations` is created by the runner, not by a migration, because the
first migration needs it to exist. It holds:

| Column | Meaning |
| --- | --- |
| `name` | The migration file name, and the primary key |
| `checksum` | SHA-256 of the file contents as applied |
| `applied_at` | When the runner recorded it |
