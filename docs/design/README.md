# Design notes

This directory holds short design notes for a single workstream whose decisions do not belong in a document that already exists.

Before writing one here, check whether the decision belongs to an existing owner:

- Cross-cutting design (configuration, secrets, data model, frontend, testing, performance, URLs, administration, packaging) belongs to the matching document under [../architecture/](../architecture).
- Event names and payloads belong to [../reference/events.md](../reference/events.md).
- A security decision belongs to [../security/threat-model.md](../security/threat-model.md).
- The plan itself, phase order, and status belong to [../implementation-plan.md](../implementation-plan.md).
- Per-phase scope and acceptance criteria belong to the roadmap file for that phase under [../roadmap/](../roadmap).

A note is justified only when none of those fit. Keep it short: the decision, the alternatives that were rejected, the reason, and the consequence for later phases. Link it from the workstream that needs it, and delete it if its content moves into an owning document.

Roadmap workstreams that already anticipate a note:

| Note | Needed before | Covers |
| --- | --- | --- |
| `actions.md` | W5.1 | Runner protocol version, queue schema, worker-to-database role |
| `cloud-development.md` | W9.1 | Runtime adapter contract, workspace state machine, quota accounting, preview-host trust model |
