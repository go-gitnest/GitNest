# Working on GitNest autonomously

This file is the entry point for an autonomous agent (or a new contributor) picking up work in this repository. It defines where the truth lives, how to choose the next task, how to verify it, and what must never be done.

## Read these first, in this order

1. [docs/product-goals.md](docs/product-goals.md) — what the product is, the parity definition, and the decided exclusions.
2. [docs/architecture.md](docs/architecture.md) — module boundaries, storage, security model.
3. [docs/implementation-plan.md](docs/implementation-plan.md) — phase order, current status, cross-cutting requirements, and "Immediate next work".
4. The cross-cutting design documents that apply to the workstream you are about to touch, listed in the implementation plan. At minimum: [architecture/data-model.md](docs/architecture/data-model.md), [architecture/secrets.md](docs/architecture/secrets.md), [architecture/testing.md](docs/architecture/testing.md), and [architecture/configuration.md](docs/architecture/configuration.md).
5. The roadmap file for the active phase: `docs/roadmap/<NN>-<area>.md`.

Before the first product feature exists, the active phase is **phase 0**, described in [docs/roadmap/00-service-foundation.md](docs/roadmap/00-service-foundation.md).

Do not start work from a phase whose dependencies are not met. The dependency order is in the implementation plan.

## Where new material goes

- **Plan and status:** [docs/implementation-plan.md](docs/implementation-plan.md).
- **Per-phase workstreams and acceptance criteria:** `docs/roadmap/<NN>-<area>.md`.
- **Cross-cutting design owned by a document already listed in the plan:** edit that document rather than creating a parallel one.
- **A design note for one workstream that does not fit an existing document:** `docs/design/<topic>.md`. Keep it short, state the decision and the tradeoff, and link it from the roadmap workstream that needs it. Do not create a design note to restate what a roadmap file already says.
- **Help output and operator instructions:** `README.md`, updated in the same change that alters commands or configuration.

## Objective for a long-running goal

If you are hanging a persisted goal on this repository, use an objective with this shape:

> Continue the GitNest implementation plan in docs/implementation-plan.md. Work the "Immediate next work" items in order, then continue through the active phase's roadmap file. For each workstream: implement it, keep authorization in shared application services, add tests covering allowed and denied paths, run the verification commands in AGENTS.md, update the workstream status and the phase table in docs/implementation-plan.md, and tick the matching lines in docs/parity-checklist.md — but only the lines whose acceptance criteria actually pass, never ahead of the work. Never commit or push; leave the working tree ready for the maintainer to review. Stop and ask the maintainer when a workstream would contradict docs/product-goals.md, change a decided exclusion, or require a product decision the documents do not answer.

## Round protocol

Each round of work does exactly this:

1. **Orient.** Read the implementation plan's "Current status" and "Immediate next work", plus the active phase's roadmap file. Check the working tree for uncommitted work from a previous round and continue it rather than starting something new.
2. **Pick one workstream.** Choose the first unblocked item in the plan order. One reviewable change per round. If a workstream is too large for one round, split it and record the split in the roadmap file.
3. **Implement.** Follow the existing code's conventions. Look at a neighbouring package before inventing a new pattern.
4. **Test.** Add or extend tests beside the code. Cover the denied path and the persistence or concurrency behaviour, not only the happy path.
5. **Verify.** Run the verification commands below and read the actual output. A claim of success without command output is not evidence.
6. **Update the documents.** Set the workstream's status in its roadmap file, update the phase table in `docs/implementation-plan.md`, and tick the matching lines in `docs/parity-checklist.md`. Tick only what this change actually finished and verified; leave everything else unticked and say so in the report. If the change alters commands, configuration, or upgrade behaviour, update `README.md`.
7. **Report.** State what changed, which files, the exact verification output, and what is next.

## Verification commands

Run these before claiming a workstream is done. The Go paths exist from W0.1 onward:

```sh
gofmt -l cmd internal migrations        # must be empty
go vet ./...                            # must exit 0
go build ./...                          # must exit 0
go test ./...                           # unit tests; integration tests skip without a database
```

With PostgreSQL available (the README documents `docker compose up -d postgres`):

```sh
# PowerShell
$env:GITNEST_TEST_DATABASE_URL = "postgres://gitnest:gitnest@localhost:5432/gitnest?sslmode=disable"
go test -race ./...
```

The integration tests create and drop their own schema, so they never touch application data. Set the environment variable to run them; leave it unset to skip them.

For changes to the service lifecycle or the CLI, also exercise the built binary:

```sh
go build -o ./gitnest ./cmd/gitnest
./gitnest migrate
./gitnest serve          # then check /api/health and /api/ready, then stop it
```

## Definition of done

See "Definition of done for a feature" in [docs/product-goals.md](docs/product-goals.md). In short: a browser flow, the API surface, authorization in the shared service, allowed and denied tests, a forward-only migration for schema changes, audit events where consequential, updated operator documentation, and no secret or repository content in any log, error, or metric.

## Hard rules

- **Tick a parity checkbox only in the change that finishes it.** [docs/parity-checklist.md](docs/parity-checklist.md) is the record of what exists. Move a line from `- [ ]` to `- [x]` in the same change that makes its acceptance criteria pass, and only then.
  - Never tick ahead of the work, and never tick because the code "mostly" works.
  - Never tick a box whose passing evidence you cannot show in the report.
  - Never leave a finished item unticked: an unticked box for working code is as wrong as a ticked box for missing code.
  - If a ticked item later regresses, untick it in the same change that reveals the regression, and say so.
- **Never commit, push, or create tags.** The maintainer reviews and commits. Leave the working tree with the change in place and describe it.
- **Never edit a migration that may already have been applied.** Add a new forward-only file in `migrations/`.
- **Never answer an authorization question in a transport.** Browser, REST, GraphQL, HTTPS Git, SSH Git, and package registries all call the same shared service.
- **Never run untrusted code in the web process**, and never give untrusted execution (Actions jobs, workspaces) a credential that reaches GitNest's database or another tenant's data.
- **Never log or return secrets**: passwords, password hashes, session or invitation tokens, personal access tokens, private keys, webhook secrets, action secrets, or repository contents.
- **Never interpolate request data into a shell command**, and never accept a filesystem path from a client.
- **Do not implement excluded features.** The mobile app, GitHub's own Codespaces, Advanced Security (code scanning, secret scanning, Dependabot), Gist, payments, and AI code features are decided non-goals. See [docs/product-goals.md](docs/product-goals.md).
- **Do not add a dependency for something the standard library handles**, and do not introduce a framework for a single route.
- **Do not create empty packages** for phases that have not started.

## When to stop and ask

Stop and ask the maintainer when a workstream would:

- contradict or require changing a decided exclusion or a product rule;
- require a product decision the documents do not answer (for example a new default policy, a pricing-like capability, or a change to the permission model);
- require a new always-on infrastructure dependency for the single-node install;
- need credentials, network access, or a hosted service that is not available in the development environment;
- discover that an implemented behaviour contradicts a document (report it, propose the correction; do not silently change behaviour).

## Environment notes

These are known constraints of the development environment, recorded so they are not rediscovered every round:

- **Integration tests are schema-scoped.** Each test creates a uniquely named schema, runs in it, and drops that schema afterwards. Drop the schema, not the database: on some PostgreSQL 18 installations `DROP DATABASE` blocks waiting on a signal barrier, which stalls test cleanup.
- **Line endings matter.** `.gitattributes` normalizes to LF and `.editorconfig` sets `end_of_line = lf`. Files written with CRLF fail the `gofmt -l` check that CI runs.
- **CI runs on Linux with a PostgreSQL service container.** Local runs on Windows are expected to match it; if they diverge, fix the portability problem rather than the CI configuration.
- **Build caches must live inside the workspace** in a sandboxed environment. If `go build` fails with an access-denied error on the module or build cache, point `GOCACHE`, `GOMODCACHE`, `GOPATH`, and `GOTMPDIR` at directories under the workspace.
- **Go 1.26 or newer is required.** The module should record `go 1.26.0`: `golang.org/x/crypto` (Argon2id) and `golang.org/x/term` (password input without echo) require it.

## Progress ledger

Three records must agree at the end of every round:

1. The "Current status" phase table and the active phase's workstream statuses in [docs/implementation-plan.md](docs/implementation-plan.md).
2. The `- [x]` state of [docs/parity-checklist.md](docs/parity-checklist.md).
3. The code in the working tree.

A round that updates one without the others is unfinished. Keep "implemented", "partly implemented", and "not started" honest: a phase with a working subset is "in progress", never "implemented". A parity box is the strongest of the three claims, so it is the last to be made and the first to be withdrawn.
