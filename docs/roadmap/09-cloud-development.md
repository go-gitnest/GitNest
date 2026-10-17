# Cloud development roadmap
**Phase:** 9
**Status:** not started
**Roadmap file:** docs/roadmap/09-cloud-development.md

## Purpose
Give GitNest its own browser-based development environment: a workspace created from a repository and branch that runs the user's code in an isolated container with a persistent volume, an editor, a terminal, and forwarded ports. It replaces GitHub Codespaces, and it also covers the scratch-space use case that GitHub serves with Gist: an empty workspace with no repository attached. Workspaces execute untrusted user code and are therefore managed by a dedicated control plane outside the web process, with no host or database credentials inside a workspace.

## Scope
Workspace lifecycle (create from repository or branch, start, stop, rebuild, delete, idle reaping), container image and devcontainer-style configuration, one persistent volume per workspace with defined rebuild semantics, browser IDE and terminal access authenticated by GitNest identity, port forwarding with preview URLs and access control, dotfiles and personal settings, prebuilds, resource quotas and fair scheduling, workspace sharing and collaboration, snapshots and restore, workspace-scoped secrets and environment variables, isolation hardening, and how workspaces interact with repository permissions.

Out of scope: GitHub's own Codespaces implementation and its billing; the mobile app; Advanced Security; Copilot and AI assistance; Gist as a separate product surface (scratch workspaces cover it).

## Workstreams

### W9.1 Workspace lifecycle and state machine
- **Deliverable:** `internal/workspace` holds one state machine over `pending`, `building`, `starting`, `running`, `stopping`, `stopped`, `failed`, `deleting`, `deleted`, with every transition persisted and emitted as an event. Create from `{repository_id, ref}` or with no repository (scratch), start, stop, rebuild, delete, and an idle reaper that selects running workspaces by last-activity timestamp and instance policy. A transition that loses a race is rejected by the database, not resolved in memory.
- **Depends on:** `internal/repository` read access and permission service; `internal/platform/postgres`.
- **Acceptance:** Tests cover every legal transition and rejection of illegal ones (for example start while deleting). A create that fails mid-build leaves a `failed` workspace with a readable reason, never a stuck `building` row. Idle reaping stops the container and keeps the volume. Two concurrent start requests for one workspace start exactly one container. A workspace whose controlling process restarts is reconciled from persisted state rather than left in a transitional state.

### W9.2 Devcontainer-style configuration and image build
- **Deliverable:** `internal/workspace/devcontainer` parses `.devcontainer/devcontainer.json` (and `devcontainer.json` at the root) with strict validation: image or build context, `features` pinned by version, `remoteUser`, `containerEnv`, `mounts`, `forwardPorts`, `postCreateCommand`, `postStartCommand`, `workspaceFolder`, and `customizations`. Builds are keyed by a content hash of the resolved configuration and build inputs, so an unchanged configuration reuses a cached image. `mounts` and build context are constrained to sources GitNest controls; a mount outside the workspace or an allowlisted cache root is rejected at parse time.
- **Depends on:** `internal/platform/container`, W9.1.
- **Acceptance:** Fixtures cover a valid configuration, an unknown key, an unsupported feature form, a `postCreateCommand` that fails (workspace reaches `failed` with the command output retained), and a mount outside the allowed roots (rejected, not silently dropped). Two creates with identical configuration reuse one cached image; a changed configuration produces a new image tag. A build that is interrupted leaves no image tagged as complete, and a cached image is reused only when the recorded content hash matches.

### W9.3 Workspace runtime and cleanup
- **Deliverable:** A `Runtime` interface in `internal/platform/container` (create, start, stop, delete, exec, inspect, stats, and network management) with a local runtime implementation for the baseline single node and an explicit "no runtime configured" capability probe. Containers, networks, and volumes are created with a workspace label, so a reconciler can find and remove orphans left by a crash. Cleanup on stop, delete, failed create, timeout, and process restart is deterministic.
- **Depends on:** W9.1.
- **Acceptance:** A reconciler test starts with manually created leftover containers plus database rows and removes exactly the orphans. Deleting a workspace removes its containers and network; the volume is removed only when the delete request says so. With no runtime configured, create fails with an actionable message and no partial state row remains. A runtime that returns an error mid-create leaves no container behind, and a reconciler run is safe to repeat.

### W9.4 Persistent volume and rebuild semantics
- **Deliverable:** One volume per workspace, keyed by the workspace UUID and never by a user-supplied name, mounted at the configured `workspaceFolder`. Stop and start preserve it; rebuild replaces the container from a new image while preserving it; a rebuild with an explicit reset flag recreates it. Disk usage is measured and enforced against the workspace quota.
- **Depends on:** W9.2, W9.3.
- **Acceptance:** Tests prove files written before a stop are present after a start and after a rebuild, and absent after a reset rebuild. A volume UUID from a deleted workspace cannot be reattached by any API input. Exceeding the disk quota fails the write path with a clear error and surfaces in the workspace status.

### W9.5 Browser IDE access
- **Deliverable:** The IDE assets are served through GitNest's own routes, not from a workspace-hosted port exposed publicly. The browser session is authorized once against the workspace access service, and the IDE connection then uses a short-lived, workspace-scoped, single-use session token bound to the workspace and user. No Git credential, token, or platform credential is ever placed in browser local storage or the URL.
- **Depends on:** W9.1, W9.3; `internal/identity` session services.
- **Acceptance:** An unauthenticated request for an IDE route is refused. A token issued for workspace A cannot open workspace B. A replayed session token is refused. Tests assert that no credential value appears in the served HTML, in local or session storage, or in the page URL.

### W9.6 Terminal sessions
- **Deliverable:** Terminal access over WebSocket from the browser through the Go server into the workspace container's PTY, supporting resize, detach, and reattach within a session lifetime, with scrollback bounded server-side and every attach/detach recorded in the audit log.
- **Depends on:** W9.5.
- **Acceptance:** An unauthorized attach is refused. Resize is applied to the PTY. A detached session reattaches to the same shell within its lifetime and is terminated after it. Closing the browser tab terminates the session and its process; no orphan shell survives past the configured grace period.

### W9.7 Port forwarding, preview URLs, and access control
- **Deliverable:** A workspace-side agent reports listening ports and their protocol. GitNest proxies each forwarded port at a per-workspace preview URL on a configured wildcard host. Access follows an explicit policy: private to the owner by default, optionally shared with workspace collaborators, optionally with a signed expiring link, and never public to the internet unless an operator enables it.
- **Depends on:** W9.3, W9.5.
- **Acceptance:** A service listening on a forwarded port is reachable through the preview URL by an authorized user and returns 404 to an unauthorized one, so port existence is not disclosed. An expired link is refused. WebSocket forwarding works. A port that stops listening removes its preview route within a bounded time.

### W9.8 Dotfiles and personal settings
- **Deliverable:** Per-user settings for a dotfiles repository, an optional install command, and a shell preference, applied once per workspace at build or first start. The install runs under the workspace user with credentials injected for that one operation and never persisted into the volume, image, or environment.
- **Depends on:** W9.2.
- **Acceptance:** A dotfiles repository is installed and its files are present in the workspace. A failing install records a warning on the workspace and does not block startup. Tests assert the credential used for the clone is absent from the process environment, the image, and the volume afterwards.

### W9.9 Prebuilds
- **Deliverable:** Prebuilds triggered by pushes to configured branches build an image plus a volume snapshot keyed by repository revision and devcontainer content hash. Workspace creation reuses a compatible prebuild or falls back to a live build. Prebuilds are pruned by age and count per repository.
- **Depends on:** W9.2, W9.4.
- **Acceptance:** A workspace created after a prebuild starts without running a build, verified by the recorded build step source. A commit that changes the devcontainer configuration does not reuse the old prebuild. Pruning keeps the configured count and deletes the corresponding volumes and images.

### W9.10 Resource quotas and fair scheduling
- **Deliverable:** Per-user, per-repository, and instance quotas for concurrent workspaces, CPU, memory, disk, and lifetime hours, plus a scheduler that queues workspace starts when instance capacity is exhausted and reports queue position. Idle and expired workspaces are stopped before new ones are refused.
- **Depends on:** W9.3.
- **Acceptance:** Tests prove a user over the concurrency limit is queued and starts after a running workspace stops. Instance capacity is never oversubscribed by concurrent admits (verified with concurrent start requests). A quota rejection names the exhausted resource. Expired workspaces are stopped rather than leaked.

### W9.11 Workspace sharing and collaboration
- **Deliverable:** A workspace collaborator list with roles (read, edit, terminal) granted by owner or repository admin, delivered as short-lived invitations, plus a link-sharing option with expiry. Live collaboration allows several authorized sessions in one workspace at once.
- **Depends on:** W9.1, W9.5.
- **Acceptance:** A collaborator with `read` cannot open a terminal or edit files; with `terminal` cannot push. Revocation takes effect on the next authorized request and terminates existing sessions within a bounded time. Two browsers attached concurrently both work and audit entries name each actor.

### W9.12 Snapshots and restore
- **Deliverable:** A snapshot records the workspace filesystem (image plus volume snapshot) with a name, creator, and size. Restore recreates a workspace from a snapshot into the same workspace or a new one. Snapshots expire by policy with an explicit deletion path and a size accounting entry.
- **Depends on:** W9.4.
- **Acceptance:** A snapshot restored into a new workspace contains the expected files and starts from the recorded image. Restore into a workspace owned by another user is refused. Expired snapshots are deleted along with their volumes, and their quota accounting is released.

### W9.13 Workspace secrets and environment variables
- **Deliverable:** Workspace-scoped and repository-scoped secrets and environment variables, encrypted at rest, injected at start into the container environment or a runtime-only file, never into the image or the persistent volume. Rotation is explicit and recorded with a revision counter. Repository-scoped values are visible to the workspace only when the requesting user's repository permission allows it.
- **Depends on:** W9.14; the secret storage introduced for Actions.
- **Acceptance:** A secret is present in the process environment inside the workspace and absent from the image layers, the volume contents, and every API response. A user without repository write access cannot read or create repository-scoped secrets. After rotation, a restarted workspace sees the new value and a running one does not.

### W9.14 Isolation hardening
- **Deliverable:** Every workspace runs as a non-root user with dropped capabilities, no privileged mode, no host path mounts, no container runtime socket, no access to the GitNest configuration directory or its own credentials, explicit CPU/memory/pids/disk limits, and an egress policy with a default-deny posture and a configurable allowlist for common package and registry hosts. A seccomp profile and, where available, a mandatory access-control profile are applied by the runtime adapter.
- **Depends on:** W9.3.
- **Acceptance:** Tests assert the container user is not root, the runtime socket is not present, the GitNest configuration path is not mounted, and a fork-bomb-style process-count test is contained. Egress to an unlisted host fails and egress to an allowlisted host succeeds. These checks run as integration tests gated by `GITNEST_TEST_DATABASE_URL` where they touch persistence, and by a runtime presence check otherwise.

### W9.15 Repository permission interaction
- **Deliverable:** One access decision path for workspaces: create from a repository requires repository read access at creation time, workspace sharing is explicit and never implied by repository access, and repository access revocation triggers re-evaluation that either stops access to repository-backed content or notifies the owner, without deleting a collaborator's own files. Authorization lives in `internal/workspace` services, called by the browser and API transports rather than reimplemented in either.
- **Depends on:** W9.1; `internal/repository` permission service.
- **Acceptance:** A user with no repository access cannot create a workspace from that repository, cannot list it, and cannot reach its preview URLs. Revoking repository access removes the collaborator's workspace access on the next request. Deleted repositories leave workspaces readable only by their owners, with the attached repository marked detached rather than half-deleted.

### W9.16 Scratch workspaces (Gist replacement)
- **Deliverable:** A workspace created with no repository: no clone, no repository permissions required, an owner-scoped volume, and an explicit export path that creates a new GitNest repository from the workspace contents when the user asks for one. Scratch workspaces are private by default and shareable through the same sharing model.
- **Depends on:** W9.1, W9.11.
- **Acceptance:** A user with no repositories can create, run, stop, and delete a scratch workspace. Export creates a repository owned by the user with the workspace contents as the initial commit, and refuses when the target name is taken. A scratch workspace is never listable or reachable by another user without an explicit share.

## Data and interfaces
- Schema additions (`migrations/NNN_cloud_development_*.up.sql`, appended forward, never editing an applied migration): `workspaces`, `workspace_events`, `workspace_volumes`, `workspace_images`, `workspace_shares`, `workspace_sessions`, `workspace_snapshots`, `workspace_prebuilds`, `workspace_ports`, `workspace_secrets`, `workspace_variables`, `workspace_quota_usage`, `workspace_reconciliations`.
- Workspace identity: a workspace is identified by a UUID and stores owner user ID, optional repository ID and ref, an image reference, a volume reference, a state, a last-activity timestamp, and the runtime container identifier. Storage paths and runtime names derive from the UUID, never from a user-supplied name or repository path.
- Volume and image rows record size, creation source (live build, prebuild, snapshot restore), and the content hash they were built from, so compatibility between a prebuild and a create request is decided by data, not by convention.
- Packages: `internal/workspace` (lifecycle, access service, prebuilds, snapshots, quotas), `internal/workspace/devcontainer`, `internal/platform/container` (shared by Actions jobs and workspaces; one runtime adapter, two callers with different policies), `internal/platform/storage` (snapshot and image artifacts), `internal/platform/postgres`, `internal/platform/logging`, `internal/transport/http/api` (workspace API), `internal/transport/http/web` (browser routes, IDE proxy, terminal WebSocket, preview proxy), `internal/identity` (session verification for proxy and terminal routes).
- API surface: `/api/v1/repos/{owner}/{repo}/workspaces`, `/api/v1/workspaces`, `/api/v1/workspaces/{id}` (get, delete), `.../start`, `.../stop`, `.../rebuild`, `.../ports`, `.../snapshots`, `.../shares`, `.../secrets`, `.../variables`, plus browser routes `/workspaces/{id}` (IDE), `/workspaces/{id}/terminal`, and preview hosts `{port}-{workspace}.<preview-host>`.
- Authorization: create, list, read, start, stop, rebuild, delete, share, and preview access all resolve through the workspace access service, which itself calls the `internal/repository` permission service when the workspace is repository-backed. No transport decides access on its own.
- Events: `workspace.created`, `workspace.started`, `workspace.stopped`, `workspace.rebuilt`, `workspace.deleted`, `workspace.failed`, `workspace.snapshot.created`, `workspace.share.changed`, emitted through the durable outbox so lifecycle reporting survives restarts.
- Configuration: container runtime endpoint, preview base host and wildcard TLS expectation, instance concurrency and per-user defaults, idle and maximum lifetime, egress allowlist, snapshot retention, prebuild branch list, dotfiles policy, `GITNEST_SECRET_KEY` for workspace secret encryption.
- Planned design spec: `docs/design/cloud-development.md`, written before W9.1 lands, covering the runtime adapter contract, the state machine, quota accounting, and the preview-host trust model.

## Security notes
- Workspace containers are the trust boundary. They execute user-controlled code, never run in the web process, and never receive database credentials, the GitNest secret key, or the container runtime socket.
- Limits are mandatory, not advisory: CPU, memory, pids, disk, wall-clock lifetime, and idle timeout. A workspace over its limits is stopped by the control plane, not asked to stop itself.
- The control plane never executes code from inside a workspace. Container output is data: it is parsed defensively, bounded in size, and never interpreted as a command, a path, or a configuration value by GitNest itself.
- Credentials are injected at start or at the specific operation that needs them, kept out of the image and the persistent volume, and never returned by an API. Git access from inside a workspace uses a short-lived, scoped token, not a long-lived personal access token.
- Port forwarding and preview URLs are authorization points: refusal must not disclose whether a port exists. Preview routing requires the same session verification as the IDE, and link sharing uses signed, expiring, revocable tokens.
- Preview routing resolves the workspace from the signed route or host mapping, never from a client-supplied container address or port on the host network.
- WebSocket terminal and IDE routes verify identity and workspace scope on every attach, not only on the initial handshake, and record actor, workspace, and time in the audit log.
- Untrusted input: devcontainer JSON, dotfiles commands, lifecycle commands, forwarded port metadata, file contents, and terminal output. Output shown in the browser is escaped; nothing from a workspace is rendered as trusted HTML.
- Rebuild and snapshot paths must not let one workspace read another's volume, image, or snapshot. Volume, image, and snapshot identifiers are resolved from database rows bound to the requesting workspace.
- A workspace is never a place where credentials accumulate: a token that was valid for one operation is not left in the environment, the shell history, or a file the next session inherits.
- A deleted user or revoked session stops workspace access on the next request, and existing IDE and terminal sessions are terminated within a bounded time rather than left running.
- Logs and metrics never contain workspace file contents, secret values, or token values; the audit trail records that a session occurred and by whom, not what was typed.

## Out of scope
GitHub's Codespaces service and billing; the mobile app; Copilot or AI assistance inside the IDE; Advanced Security scanning of workspace contents; Marketplace distribution of devcontainer features beyond pinned public references; Gist as a distinct product surface.

## Exit criteria
A user with repository access can create a workspace from a branch, get an IDE and a terminal in the browser, forward a port and reach it through an access-controlled preview URL, stop the workspace, and start it again with the persistent volume intact. A push to a prebuild-enabled branch makes the next workspace start without a build. A scratch workspace can be created without any repository and exported to a new repository. Every workspace runs outside the web process under enforced CPU, memory, disk, process, and lifetime limits with no host or database credentials, and is removed deterministically on stop, delete, idle reap, and process restart. Revoking repository access or a session removes workspace access without leaving a running container behind. Integration tests gated by `GITNEST_TEST_DATABASE_URL`, plus runtime integration tests where a container runtime is available, cover both the allowed and denied cases for workspace access, sharing, terminal attachment, preview URLs, repository permission interaction, and isolation assertions.

## Dependency order

The order below is a dependency order, not a schedule. The isolation boundary and the runtime adapter come first because everything else assumes a container that can be created, limited, and destroyed reliably.

```text
W9.3 runtime + cleanup -> W9.1 lifecycle -> W9.4 volume + rebuild
        |                      |                    |
        v                      v                    v
 W9.14 isolation        W9.5 IDE access      W9.9 prebuilds
        |                      |                    |
        v                      v                    v
 W9.10 quotas           W9.6 terminal        W9.12 snapshots
                               |
                               v
                        W9.7 ports + preview
                               |
                               v
                 W9.2 devcontainer, W9.8 dotfiles, W9.13 secrets
                               |
                               v
                 W9.11 sharing, W9.15 permissions, W9.16 scratch
```

Two constraints matter more than the order itself. W9.3 and W9.14 must land before a workspace is ever exposed to a user other than its creator, because isolation cannot be added to a running design without rebuilding the container contract. W9.15 must land with W9.11 rather than after it, because sharing that is not tied to the single access decision path becomes a second authorization system that drifts from the first.
