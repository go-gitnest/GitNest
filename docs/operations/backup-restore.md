# Backup and restore

This document is the procedure. It is written so that somebody who did not create the
backup can restore it: every command is given in full, every value you have to
supply is named, and the limits of the procedure are stated rather than implied.

The phase-1 procedure covers the **single-node** deployment described in
[../architecture.md](../architecture.md): one `gitnest` binary, PostgreSQL, and the
repository storage on local disk. A multi-node deployment with object storage needs
the coordinated procedure in
[../roadmap/10-operations-and-scale.md](../roadmap/10-operations-and-scale.md)
W10.11, which extends this one.

## What a recovery set is

A recovery set is **one directory** containing both halves of the instance:

```
<set>/
  manifest.json      what the set holds, and the evidence that it is complete
  database.dump      the PostgreSQL dump (pg_dump custom format)
  repositories/      a copy of the repository root
```

The repository root holds the bare Git repositories **and the LFS objects**, because
the LFS store lives under `<repository root>/lfs`. Copying that one directory is why
there is no second location an operator can forget.

[../architecture.md](../architecture.md) section 8 states the rule this satisfies:
"Database and repository-storage backups are one recovery set with one documented
procedure."

### What the set is verified against

`manifest.json` records the dump's size and SHA-256, the total file count and byte
size of the storage copy, and one entry per repository the database knew about. Every
one of those is checked, which is what turns "the command exited zero" into "the set
is complete".

### What verification does **not** prove

Worth stating plainly, because a verification that overstates itself is worse than
none:

- It proves the files are present, the dump matches its recorded digest, and each
  repository's file count and byte size still match the manifest.
- It does **not** prove a repository's object graph is undamaged. Use `git fsck` for
  that; step 5 of the drill below runs it.
- It does **not** prove the dump restores. Only a restore into a clean instance
  proves that, which is what the drill is.

## The configuration you need

Both commands read the instance's normal configuration, so the same environment that
runs `gitnest serve` works:

| Variable | Used for | Default |
| --- | --- | --- |
| `GITNEST_DATABASE_URL` | the database to dump and restore | `postgres://gitnest:gitnest@localhost:5432/gitnest?sslmode=disable` |
| `GITNEST_REPO_ROOT` | the repository root to copy and restore into | `<data dir>/repositories` |
| `GITNEST_TMP_ROOT` | refused as a backup target | `<data dir>/tmp` |

**No command takes a password, a token, or a DSN as an argument.** That is why there
is no `--database-url`: arguments are visible in the process table and in shell
history, so the connection string comes from the environment and the password is
moved into the child process's environment rather than onto its argument vector.

The `pg_dump`, `pg_restore`, and `psql` programs must be on `PATH`, and their major
version must be at least the server's. On Windows an installer does not add them to
`PATH` by default; add `<PostgreSQL install>\bin` yourself.

## Backing up

Stop nothing. This procedure is safe to run against a live instance, and the next
section explains what that costs you.

```sh
gitnest backup --target /srv/backups/gitnest-2026-10-14
```

The target must satisfy three rules, and the command refuses it otherwise:

1. **It must not exist, or must be empty.** A set is one capture; a directory holding
   two intermixed captures is not a recovery point.
2. **It must not be inside the repository root.** Otherwise a later capture would copy
   the previous set into the next one, and every backup would carry every earlier one.
3. **It must not be inside the temporary root.** The instance's startup sweep deletes
   what it finds there, so the backup would be removed by the next restart.

On success the command prints the manifest summary and `verified yes`. **Check for
that line.** Everything else on stdout is informational.

### What the capture does not do

These are recorded in the set's own manifest as well, so they travel with it:

- **It is not point-in-time consistent across the two halves.** The database is
  dumped first, then the storage is copied. A write that lands between the two can be
  in one half and not the other. For a repository pushed during the capture, the row
  and the bytes usually both survive — the row is written first — but a push that
  begins after the row was dumped lands only in the storage copy, where a restore
  would leave an unreferenced directory that `gitnest admin check` reports as a
  finding.
- **There is no point-in-time recovery and no incremental capture.** A set is a full
  copy. Restoring it returns the instance to the moment of the capture and no later.
- **The instance secret key is not in the set.** See below.

### The secret key is separate, deliberately

[../architecture/secrets.md](../architecture/secrets.md) requires the key to be kept
"separately from the database backup, because a backup that contains both the
ciphertext and the key protects nothing". Nothing in the backup procedure reads it.

So the recovery set is **not** sufficient on its own for a future phase that stores
replayable secrets. Back the key up by whatever mechanism your organisation uses for
key material, store it separately from the sets, and record where it is. Phase 1
stores no replayable secret, so today nothing depends on it — but the separation is
the rule from the start rather than a habit added later.

### Protect the sets

A recovery set is the most sensitive data the instance holds: the full database,
including password hashes and every repository's contents.

- The capture creates `0700` directories and `0400` files.
- **On Linux the file modes are the protection. On Windows they are not**: NTFS does
  not carry POSIX permission bits, so a directory reports `0777` whatever mode was
  requested. What Windows does honour is the read-only attribute, which the procedure
  sets on every file — so a set cannot be edited in place, but the directory's access
  is whatever its parent grants. On Windows, restrict the directory with an ACL.
- Encrypt the destination if it is not already on encrypted storage. A set is a
  candidate for the same handling as a database dump.

## Verifying a set

Any time, read-only, safe against a running instance:

```sh
gitnest backup-verify --source /srv/backups/gitnest-2026-10-14
```

It reports every problem it finds rather than the first, and it separates two kinds,
because they call for different responses:

- **Problems that make the set unusable** — a missing dump, a digest that does not
  match, a truncated storage copy, a repository the manifest says was present and the
  set does not hold. The exit status is non-zero and the set must not be relied on.
- **Findings about the instance**, reported on stderr: a repository the database
  references whose directory did not exist when the capture ran. The set is complete —
  it holds everything that existed — but the instance was already inconsistent, and
  the capture is exactly when you want to learn that.

Run this against every set before you need it. An unverified backup is a hope.

## Restoring

**Stop the instance first.** Restoring writes into the repository root and into the
database, and it is not safe against a running server.

```sh
systemctl stop gitnest
```

Then:

```sh
gitnest restore --source /srv/backups/gitnest-2026-10-14 --confirm
```

`--confirm` is required. Without it the command prints its usage and refuses, because
the operation overwrites data and cannot be undone.

The command:

1. **Verifies the set first.** A restore never begins overwriting an instance from a
   set that turns out to be incomplete.
2. Restores the database (`pg_restore --clean --if-exists`, so re-running an
   interrupted restore is safe and idempotent).
3. Restores the repository root.
4. Tells you to run `gitnest migrate` and `gitnest admin check`.

Then, in this order:

```sh
gitnest migrate
gitnest admin check
systemctl start gitnest
```

`gitnest migrate` matters because the restored database is at the schema version the
set recorded, which may be older than the running binary. The migration runner applies
the difference; it refuses if the database is *newer* than the binary, which means you
have restored a set from a newer release and need that release's binary.

## The drill

Run this into a scratch instance, not over production. It is the procedure the
acceptance criterion describes: somebody who did not create the set can restore it and
clone what comes back.

1. **Take a set:**
   ```sh
   gitnest backup --target /srv/backups/drill-$(date +%F)
   ```
   Confirm the output ends with `verified yes`.

2. **Verify it independently:**
   ```sh
   gitnest backup-verify --source /srv/backups/drill-$(date +%F)
   ```
   Exit status 0, and no problems reported.

3. **Restore it into a clean instance.** A second machine, or a second data directory
   and database on this one:
   ```sh
   GITNEST_REPO_ROOT=/srv/drill/repositories \
   GITNEST_DATABASE_URL=postgres://gitnest:gitnest@localhost:5432/gitnest_drill?sslmode=disable \
   gitnest restore --source /srv/backups/drill-$(date +%F) --confirm
   gitnest migrate
   ```

4. **Check the instance agrees with itself:**
   ```sh
   gitnest admin check
   ```
   Expect no findings. A finding here means the set and the storage disagree, and
   `admin check`'s output names which.

5. **Check a repository's object graph:**
   ```sh
   git fsck --strict /srv/drill/repositories/<aa>/<bb>/<storage_id>.git
   ```
   No errors. This is the step verification cannot do for you.

6. **Clone it.** Start the instance against the restored data and clone a repository
   over HTTP with a real client:
   ```sh
   git clone http://localhost:8080/<owner>/<repo>.git /tmp/drill-clone
   ```
   The clone succeeds and the content is what you expect.

7. **Record the numbers.** How long the restore took, and the capture time of the set
   you restored. Those are the instance's observed recovery time and recovery point;
   [../roadmap/10-operations-and-scale.md](../roadmap/10-operations-and-scale.md)
   W10.12 turns them into published objectives. Repeat quarterly.

## When something goes wrong

| Symptom | Cause | What to do |
| --- | --- | --- |
| `the backup target is not a usable location` | the target is inside the repository root or the temporary root | choose a path outside both |
| `the backup target already holds a set` | the directory is not empty | use a new directory; sets are not merged |
| `the PostgreSQL client program is not on PATH` | `pg_dump` or `pg_restore` is missing | install the client tools and add `bin` to `PATH` |
| `the recovery set is incomplete or inconsistent` | verification failed | run `gitnest backup-verify` for the full list; take a new set |
| `restoring overwrites the instance's data` | `--confirm` was omitted | stop the instance and pass `--confirm` |
| `the set is format N and this build reads M` | the set was written by a newer release | use that release's binary to restore it |
| The restored instance reports a repository with no storage | the repository was pushed during the capture, or its directory was already missing | `gitnest admin check` names it; the repair path fixes the empty case |
