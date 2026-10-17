# Configuration and instance settings

**Status:** Target design. Nothing in this document is implemented yet.
**Scope:** How GitNest is configured, where each kind of setting lives, and how it changes at runtime.

## Purpose

GitNest accumulates settings quickly: bootstrap values (database, bind addresses, storage roots), instance policy (registration, default visibility, two-factor enforcement, quotas), infrastructure that only some deployments need (object storage, SMTP, a container runtime, an identity provider), and per-entity settings (organization, repository, user, workspace). Without a stated rule, every phase invents its own mechanism and the operator experience fragments.

This document fixes one rule: **infrastructure is configured at startup, policy is stored in the database, and both are validated the same way.**

## The three layers

| Layer | Lives in | Changes | Examples |
| --- | --- | --- | --- |
| Startup configuration | Environment variables, optionally a config file | Requires a restart | Database URL, HTTP bind address, SSH listen address, repository storage root, public URL, log level, shutdown timeout, container runtime endpoint |
| Instance settings | Database (`instance_settings`), edited by an operator | Takes effect without a restart | Registration mode, default repository visibility, two-factor enforcement, allowed e-mail domains, per-instance quotas and rate limits, SMTP sender, object storage bucket, search backend, runner registration policy, workspace limits |
| Scoped settings | Database rows owned by an entity | Owned by that entity's admins | Organization policy and inherited rulesets, repository visibility and protection, user notification preferences, environment protection rules, workspace configuration |

Two rules keep the layers from competing:

1. **A setting lives in exactly one layer.** It is never read from both.
2. **A startup value may seed an instance setting once**, at migration or first boot, and from then on the instance setting is authoritative. The operator can see the seeded value in the console.

## Precedence and overrides

- Startup configuration is read once at process start, validated as a whole, and never re-read. A bad value fails startup with a message naming the variable.
- Instance settings are read through a cached accessor with a short time to live, and invalidated explicitly when the console writes them, so a change is visible on the next request rather than after a restart.
- A small, explicitly listed set of startup variables may override the corresponding instance setting for automation and testing (for example forcing registration closed). Overrides are logged at startup, and the console shows the effective value and marks it as overridden. Nothing else may override.

## What may never be an instance setting

- The database URL, the repository storage root, the bind addresses, and the public URL: these define the process and the instance's identity, and changing them at runtime would mean the running process and the stored data disagree.
- Anything that is itself a secret: secrets are configured at startup or stored encrypted, never as plaintext instance settings. See [secrets.md](secrets.md).

## Instance settings storage

A single table, one row per key:

| Column | Type | Notes |
| --- | --- | --- |
| `key` | `text` primary key | Namespaced, for example `registration.mode` |
| `value` | `jsonb` not null | Typed by the registry entry, not free-form |
| `updated_at` | `timestamptz` not null | Set by the server |
| `updated_by` | `uuid` null | The administrator who changed it, `NULL` when seeded |

Settings are defined in code by a registry entry: key, type, default, whether it is mutable, validation, and the phase that introduces it. A value in the table that has no registry entry is ignored and reported as an unknown setting, so an older binary tolerates a newer row. A registry entry with no row uses its default, so a new setting needs no data migration.

## Validation

- Startup configuration is validated as one unit before the process serves traffic, and every failure names the offending variable and the accepted shape.
- Instance setting writes are validated by the registry entry before the row is written; an invalid write changes nothing and returns a field-level error to the console.
- Every setting has a default that keeps the single-node install working. A setting whose absence would break a deployment is a startup value instead.

## Operator surface

The operator console (see [administration.md](administration.md)) is the supported way to change instance settings, and it must show for each setting: current effective value, whether it is overridden by the environment, the default, what it affects, and when it last changed and by whom. Changing a setting writes an audit event.

## Settings inventory by phase

The registry is the single list; this table is the expectation of what accumulates.

| Phase | Settings introduced |
| --- | --- |
| 1 | Registration mode, default repository visibility, invitation lifetime, session lifetime, local Git operation limits |
| 2 | Notification and e-mail delivery, allowed e-mail domains, organization creation policy |
| 4 | API rate limits, token lifetime policy, webhook delivery limits, app installation policy |
| 5 | Runner registration policy, default job limits and quotas, artifact and cache retention, egress policy |
| 6 | Package retention and quotas, storage backend selection |
| 7 | Search backend selection, indexing limits, Pages and wiki policy |
| 8 | Two-factor enforcement, SSO endpoints and certificates, SCIM, audit retention, moderation policy |
| 9 | Workspace image allow-list, workspace quotas and idle reaping, port-forwarding policy |
| 10 | Read-replica and cache selection, multi-node coordination |

## Testing obligations

- Configuration loading: each variable validated for its accepted and rejected shapes, including the public URL rules.
- Instance settings: a missing row yields the registry default; an unknown row is ignored without failing; an invalid write is rejected and leaves the previous value intact; a write is visible on the next read without a restart.
- Overrides: an environment override takes effect, is logged, and is reported as overridden in the console.
- Precedence: a setting is never read from two layers.

## Open questions

- Whether a config file is offered in addition to environment variables. The decision is deferred until the settings surface is large enough to justify it; if added, it must not become a second source of truth for startup values.
