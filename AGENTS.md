# Working on GitNest (agent notes)

This file is local working notes for an AI coding assistant. It is in `.gitignore` and is **not** part of
the project. The committed, human-facing version of everything about how to work here is
[CONTRIBUTING.md](CONTRIBUTING.md) — **read that first, and treat it as authoritative for project rules.**
This file only records what is specific to working through an agent harness.

## Authority

- **Project rules, conventions, the non-negotiables, and the progress ledger live in
  [CONTRIBUTING.md](CONTRIBUTING.md).** Do not restate them here; a second copy goes stale and then
  contradicts the first. If something in this file ever conflicts with CONTRIBUTING.md, CONTRIBUTING.md
  wins.
- The documents to read, in order, are listed in CONTRIBUTING.md. The short version: product-goals →
  architecture → implementation-plan → the applicable cross-cutting design docs → the active phase's
  roadmap under `docs/roadmap/`.

## Objective for a long-running goal

> Continue the GitNest implementation plan in docs/implementation-plan.md. Work the "Immediate next work"
> items in order, then continue through the active phase's roadmap file. For each workstream: implement it,
> keep authorization in shared application services, add tests covering allowed and denied paths, run the
> verification commands, update the workstream status and the phase table in docs/implementation-plan.md,
> and tick the matching lines in docs/parity-checklist.md — but only the lines whose acceptance criteria
> actually pass, never ahead of the work. Never commit or push; leave the working tree ready for the
> maintainer to review. Stop and ask the maintainer when a workstream would contradict
> docs/product-goals.md, change a decided exclusion, or require a product decision the documents do not
> answer.

## Round protocol

One round does one reviewable change:

1. **Orient.** Read the plan's "Current status" and the active phase's roadmap. Inspect the working tree for
   uncommitted work from a previous round and continue it rather than starting something new.
2. **Pick one workstream.** The first unblocked item in plan order. If it is too large for one round, split
   it and record the split in the roadmap.
3. **Implement**, following the conventions of the neighbouring packages rather than inventing a pattern.
4. **Test** beside the code, covering the denied path and the persistence or concurrency behaviour.
5. **Verify** with the commands in README.md, and read the actual output.
6. **Update the records** — roadmap status, plan table, parity checklist — and update README.md if commands
   or configuration changed.
7. **Report** what changed, which files, the verification output, and what is next.

## Editing discipline

- **Never replace a whole file with `substring(before a marker) + new text`.** This destroyed most of the
  phase-2 roadmap once, and `docs/` was excluded from version control at the time so it could not be
  recovered. Use `edit` with the smallest unique `old_string`: it fails loudly when the match is missing or
  not unique, whereas string surgery has no guard at all. Read a file's length before and after any
  whole-file write.
- `docs/` **is** version-controlled now, but no test reads it, so nothing will catch a documentation mistake.
- **Never commit, push, or create tags.** Stage and describe; the maintainer commits.

## Harness and environment notes

Things specific to running here, recorded so they are not rediscovered every round:

- **Never pipe `go test` into a truncating filter.** `Select-Object -First N` closes the pipe once it has
  enough lines, killing `go test` before `t.Cleanup` runs and leaving a `gitnest_test_*` schema behind.
  Redirect to a file and read it, or use a filter that consumes all input.
- **A leftover schema has three causes, and they look different.** An interrupted pipe (above) leaves an
  empty one; a panic leaves a populated one; a harness interruption leaves either.
- **A harness interruption has two shapes, and the first diagnosis of them was wrong.** If the test binary is
  killed, `go test` prints a bare `FAIL <package> <elapsed>` with no `--- FAIL`, no dump and no `panic:` —
  the elapsed time is the wall-clock gap. If it survives the suspension, Go's own `-timeout` fires and
  produces a genuine `panic: test timed out` **with** a dump, while the named test's own elapsed time shows
  the gap (one was `(46h15m30s)`). **The reliable tell is that elapsed value: hours or a day means a
  suspension, at-or-just-past the timeout means a real hang. Re-run the named test alone before
  investigating.**
- **A terminated run can leave a PostgreSQL backend stuck on `IPC`** — a parallel query whose workers died
  with it — after which catalog queries hang and leftover schemas cannot be dropped. Find it with
  `SELECT pid, wait_event_type FROM pg_stat_activity WHERE wait_event_type IS NOT NULL`, terminate that
  pid, then drop the schema. Set `PGOPTIONS=-c statement_timeout=...` when probing so a stuck query fails
  instead of hanging the shell.
- **`psql` NOTICEs arrive through the error stream in PowerShell**, so a `.Trim()` in a formatting
  expression throws `MethodNotFound` while the statement actually succeeded. Read the result line.
- **The development database runs on port 60000**, outside the Windows Hyper-V reserved ranges.
  `postgres.exe -D .tmp/pgdata -p 60000 -c listen_addresses=127.0.0.1 -c fsync=off`, and
  `GITNEST_TEST_DATABASE_URL=postgres://gitnest@127.0.0.1:60000/postgres?sslmode=disable`. If PostgreSQL
  refuses to bind at all, check `netsh interface ipv4 show excludedportrange protocol=tcp` **unfiltered** —
  an earlier filtered check hid the answer for eight rounds.
- **Vite and Vitest cannot run here**: esbuild's Node wrapper spawns its platform binary over stdio pipes,
  which the sandbox refuses (`spawn EPERM`). `npm run typecheck` and `npm run lint:strings` are pure Node and
  do run. The build and client suite run in the `web-build` CI job. `npm_config_cache` must point inside the
  workspace.
- **Set `GIT_CONFIG_GLOBAL` to the null device on every git invocation a test makes.** The developer's
  `~/.gitconfig` sets an HTTP proxy that is usually not running, so git's HTTP requests fail with
  `Failed to connect ... via 127.0.0.1` while a plain HTTP client reaches the same listener fine. This has a
  time dimension: suites pass while the proxy runs and fail after it stops.
- **A test fixture cannot be built with `git clone` or `git add`** where a clean filter applies, because
  `sh.exe`'s signal pipe is refused. Build fixtures with plumbing — `hash-object -w --stdin`, `mktree`,
  `commit-tree`, `update-ref` — and clone them over HTTP against an `httptest` server.
- **Build caches must live inside the workspace**: point `GOCACHE`, `GOMODCACHE`, `GOPATH`, and `GOTMPDIR`
  at directories under `.tmp/`.
