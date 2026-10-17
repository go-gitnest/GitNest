# Contributing to GitNest

GitNest is built one reviewable workstream at a time, and the documents in `docs/` are the project's
memory. Read them before writing code: they record what the product is meant to be, what has already been
decided, and — just as important — what has been deliberately left out.

## Where the truth lives

Read in this order:

1. [docs/product-goals.md](docs/product-goals.md) — what the product is, the parity definition, the decided
   exclusions, and the definition of done for a feature.
2. [docs/architecture.md](docs/architecture.md) — module boundaries, storage, and the security model.
3. [docs/implementation-plan.md](docs/implementation-plan.md) — phase order, current status, and what is next.
4. The cross-cutting design documents that apply to what you are about to touch, listed in the
   implementation plan. At minimum [architecture/data-model.md](docs/architecture/data-model.md),
   [architecture/testing.md](docs/architecture/testing.md),
   [architecture/secrets.md](docs/architecture/secrets.md), and
   [architecture/configuration.md](docs/architecture/configuration.md).
5. The roadmap file for the phase you are working in, under [docs/roadmap/](docs/roadmap).

[docs/parity-checklist.md](docs/parity-checklist.md) is the auditable inventory behind the 1:1 promise, and
[README.md](README.md) covers installing, running, and verifying the project.

## Picking up work

The implementation plan lists the next unblocked workstream. Take that one, rather than something further
down the list: phases are ordered by dependency, and a later workstream generally assumes the
authorization and storage contracts an earlier one establishes.

If a workstream is too large to review as one change, split it, and record the split in the roadmap file so
the next person sees the same structure you did.

## Verification

[README.md](README.md#development) documents the commands, the environment variables that switch on the
database- and Git-backed tests, and what CI runs. Run them and read the output — a claim that something
works without the command output behind it is not evidence, which is a standard this project holds itself
to everywhere.

## Recording progress

Three records have to agree at the end of any change:

1. The phase table and the workstream statuses in [docs/implementation-plan.md](docs/implementation-plan.md).
2. The checkbox state in [docs/parity-checklist.md](docs/parity-checklist.md).
3. The code itself.

**A parity checkbox is the strongest claim the project makes, so it is the last one to be made and the first
one to be withdrawn.** Move a line from `- [ ]` to `- [x]` in the same change that makes its acceptance
criteria pass, with the evidence in the change description.

- Never tick ahead of the work, and never tick because something "mostly" works.
- Never tick a line whose passing evidence you cannot show.
- An unticked line for working code is as wrong as a ticked line for missing code — if you finished it, tick
  it.
- If something regresses, untick it in the same change that reveals the regression, and say so.

Keep "implemented", "partly implemented", and "not started" honest. A phase with a working subset is
"in progress", never "implemented".

## Definition of done

A feature is done when it has all of the following, not when the happy path renders:

- a browser flow and the API surface that expose it;
- authorization decided in the shared application service, never in a transport — the browser, REST,
  GraphQL, the Git transports, and package registries all call the same service, so a rule enforced in one
  of them is a rule missing from the others;
- tests covering the denied path as well as the allowed one, and the persistence or concurrency behaviour
  rather than only the first successful call;
- a forward-only migration for a schema change, and an audit record where the change is consequential;
- operator documentation updated in the same change that alters commands, configuration, or upgrade
  behaviour.

## The non-negotiables

These are constraints the product is built on rather than preferences. A change that breaks one of them
needs a decision recorded in `docs/` first.

- **Never log or return a secret.** That covers passwords, password and token hashes, session and invitation
  tokens, personal access tokens, private keys, webhook and action secrets, and repository contents. Error
  messages and metrics are subject to the same rule as logs.
- **Never answer an authorization question in a transport.** See the definition of done above.
- **Never run untrusted code in the web process**, and never give untrusted execution — Actions jobs,
  cloud-development workspaces — a credential that reaches the database or another tenant's data.
- **Never interpolate request data into a shell command**, and never accept a filesystem path from a client.
  Paths are resolved from database identifiers, not from request strings.
- **Never edit a migration that may already have been applied.** Add a new forward-only file. A released
  migration is a historical fact, and an edited one silently diverges from every instance that ran the
  original.
- **Do not implement an excluded feature.** The mobile app, GitHub's own Codespaces, Advanced Security (code
  scanning, secret scanning, Dependabot), Gist, payments, and AI code features are decided non-goals; see
  [docs/product-goals.md](docs/product-goals.md).
- **Do not add a dependency for something the standard library handles**, and do not introduce a framework
  for a single route. A dependency that parses untrusted input deserves its own justification in the change,
  because it is a parser you are trusting with hostile bytes.

## Conventions worth knowing before you read the code

- **A page must be usable before hydration.** Mutation controls on a server-rendered page are plain forms
  that post to dedicated paths; the operation travels in an `action` field so the routes stay as documented.
  See [architecture/frontend.md](docs/architecture/frontend.md).
- **No inline style strings assembled from user data**, and **colour is never the only carrier of meaning**.
  A label's colour and a milestone's progress are rendered as text, and "overdue" is a word rather than a
  red bar.
- **Untrusted text is rendered, never trusted.** Markdown goes through the sanitizer, and structured
  repository content — templates, workflow files, CODEOWNERS — is decoded strictly: duplicate keys, unknown
  keys, and unsupported syntax are refused rather than tolerated.
- **A refusal is visible rather than silent.** When content is rejected, the reason is recorded somewhere a
  person can read it, because a file that silently does not appear is indistinguishable from a bug.
- **Exit codes distinguish a mistyped invocation from a failed run**: `0` for success, `1` for a command
  that ran and failed, `2` for a usage problem. A script can then tell the two apart. See
  `cmd/gitnest/main.go`.

## When to stop and ask

Open an issue rather than guessing when a change would:

- contradict a decided exclusion or a product rule, or require changing one;
- require a product decision the documents do not answer — a new default policy, a new capability with
  pricing-like effects, or a change to the permission model;
- add a new always-on infrastructure dependency to the single-node install;
- discover that an implemented behaviour contradicts a document. Report it and propose the correction;
  do not quietly change the behaviour to match the code, or the document to match the code, without saying
  which one you believe is right.

## A note on the documents themselves

`docs/` is a deliverable, not scratch space. It is the only place the reasoning behind the code is written
down, and it is deliberately version-controlled: an earlier revision of this project lost a large part of
the phase-2 roadmap to an editing mistake, and it was unrecoverable because the directory was excluded from
version control at the time.

So treat a document edit with the same care as a code edit, and more care in one respect — the test suite
does not read `docs/`, so nothing will catch a mistake there. Change as little as the change needs, and
prefer editing a passage to rewriting a section.
