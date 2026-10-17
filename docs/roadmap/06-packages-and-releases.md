# Packages and releases roadmap
**Phase:** 6
**Status:** not started
**Roadmap file:** docs/roadmap/06-packages-and-releases.md

## Purpose

Releases turn a Git tag into a downloadable artifact with human notes, and package registries let GitNest host the artifacts that CI already produces. Both are storage features first: they add a second large-object store beside bare repositories and therefore force an explicit decision about quotas, retention, cleanup, and where bytes live. This phase implements releases and one container registry plus the common language registries on top of a single package-handler abstraction, so each ecosystem is an adapter rather than a subsystem. Local disk is the only storage backend required; an S3-compatible backend is an optional later adapter behind the same interface.

## Scope

- Releases: tags, release notes (including generated notes from commits and merged pull requests), draft and prerelease states, release assets with upload and download, assets API and browser UI.
- A container registry speaking the OCI distribution specification: manifests, blobs, tags, referrers, token auth, and pull/push over the standard Docker client.
- Package registries: npm, Maven/Gradle, NuGet, RubyGems, Composer, Helm, and a generic HTTP registry.
- Cross-cutting package concerns: authentication, permissions, visibility, retention and cleanup policies, quotas, usage accounting, audit events, and storage layout.
- Phase 5 supplies the workflow that publishes packages; this phase supplies the endpoints those workflows publish to.

## Workstreams

### W6.1 Release model, tags, and notes
- **Deliverable:** A release domain in `internal/repository` binding a release to a repository, a tag name, and the exact commit object ID, with title, body, author, `draft`, `prerelease`, `latest`, and timestamps. Tag creation through the API creates an annotated tag through the bounded Git runner; a release may also target an existing tag. Release bodies are untrusted Markdown rendered with raw HTML disabled.
- A release always records the commit object ID it was published against, so a later tag move does not silently change what the release describes. Deleting a release does not delete its tag unless deletion is requested explicitly, and that distinction is visible in the UI.
- **Depends on:** Stage 1.4 Git runner; stage 1.3 permission service; `internal/repository`.
- **Acceptance:** Tests cover release creation with a new tag, with an existing tag, with a missing ref (rejected), duplicate tag names (rejected), and a lightweight tag given as the target (rejected unless converted). A draft release is invisible to anonymous readers and to users with only read access. A bad tag name or an unresolvable ref never creates a Git ref. Moving a tag afterwards leaves the existing release's recorded commit unchanged.

### W6.2 Generated release notes and prerelease/latest semantics
- **Deliverable:** Note generation that lists merged pull requests and commits since the previous release tag, grouped by label with an explicit "other changes" bucket, plus contributor acknowledgement. `latest` is computed as the newest non-draft, non-prerelease release and is not user-assignable. Prerelease releases do not become `latest` and are excluded from the "latest release" UI slot.
- **Depends on:** W6.1; `internal/pullrequest`.
- **Acceptance:** Tests with a fixed commit graph produce deterministic notes; promoting a prerelease to a full release changes `latest`; marking a release as `latest` through the API is rejected as a read-only derivation.

### W6.3 Release assets: upload, download, and limits
- **Deliverable:** Asset upload with a per-asset size cap (default 2 GiB, operator-configurable), a per-release asset-count cap, declared content type validation, and an operator-configurable total-per-repository byte limit. Downloads stream from disk with `Content-Disposition`, `Content-Length`, and ranged requests. Replacing an asset under an existing release records the replacement instead of creating a duplicate; deleting an asset is an audited operation.
- **Depends on:** W6.1; `internal/platform/storage`.
- **Acceptance:** Tests cover an upload exceeding the cap being rejected before bytes reach final storage, a partial upload leaving no orphan file, ranged download correctness, a second upload of the same asset name replacing the first rather than listing twice, and an anonymous download of an asset on a private repository being denied with `404`.

### W6.4 Package registry abstraction
- **Deliverable:** An `internal/packages` domain with: a package identity (registry kind, owner, package name, version), a `Registry` interface implemented per ecosystem, and a `BlobStore` interface with a local-disk implementation. Blobs are content-addressed, written to a temporary path and renamed into place, and referenced by digest. A package version records the digest set it needs plus metadata, so deleting a version is a metadata operation followed by a reference-counted blob sweep.
- **Depends on:** `internal/platform/storage`.
- **Acceptance:** Tests prove an interrupted upload leaves no blob reachable from a version record, two versions sharing a layer store one blob, and deleting one of them does not remove the shared blob. The `Registry` interface has no HTTP types in its signature.

### W6.5 Container registry (OCI/Docker)
- **Deliverable:** OCI distribution v2 endpoints under `/v2/`: blob upload in monolithic and chunked modes, blob existence checks, manifest PUT/GET/HEAD by tag and digest, tag listing, manifest deletion, and `_catalog`. Manifest and config media types are validated, and a manifest referencing an absent blob is rejected. Token authentication uses the W4.8 credential model with a `WWW-Authenticate` challenge.
- **Depends on:** W6.4; W4.8.
- **Acceptance:** An integration test pushes an image with `docker push`, pulls it into a clean daemon, and verifies the digest; a push of a manifest whose layer digest is absent returns `400` with the documented error body; `docker pull` against a private package with no credential is denied.

### W6.6 npm registry
- **Deliverable:** npm endpoints: packument GET (full and abbreviated), tarball GET, publish PUT, unpublish, dist-tags, and owner management. Scope handling maps `@scope/name` to the owner namespace; publish requires the W4.11 package-write decision. Tarball integrity is recorded and served with the `shasum` clients verify.
- **Depends on:** W6.4; W4.11.
- **Acceptance:** An integration test publishes a package with `npm publish --registry`, installs it in a clean directory, and resolves a dist-tag; publishing over an existing version is rejected unless it is a forced overwrite by an authorized principal; the tarball integrity field matches the served bytes.

### W6.7 Maven and Gradle registry
- **Deliverable:** Maven repository layout with `PUT` for artifacts and checksums, `GET` for artifacts and `maven-metadata.xml`, and snapshot version handling including unique snapshot timestamps. Gradle consumes the same paths; no Gradle-specific endpoint is added.
- **Depends on:** W6.4; W4.11.
- **Acceptance:** An integration test deploys a jar with checksums and resolves it with Gradle from a clean cache; a `PUT` without a matching checksum file is accepted but flagged, while a checksum mismatch on an existing artifact is rejected; publishing a release version that already exists is rejected.

### W6.8 NuGet registry
- **Deliverable:** NuGet v3 service index, package publish, flat-container versions and download, package metadata, and symbol package (`.snupkg`) upload. Package IDs are case-insensitive for lookup and case-preserving for display.
- **Depends on:** W6.4; W4.11.
- **Acceptance:** An integration test restores a package with `dotnet restore` against a clean NuGet cache; a duplicate ID/version push is rejected with the documented status; a lookup for a differently cased ID resolves the same package.

### W6.9 RubyGems registry
- **Deliverable:** RubyGems endpoints: gem push, yank, `specs.4.8.gz` and `latest_specs.4.8.gz` index generation, the compact index endpoints (`/info/<gem>`), and gem download. Index regeneration is a durable job rather than work in the request path.
- **Depends on:** W6.4; `internal/jobs`.
- **Acceptance:** An integration test pushes a gem and resolves it with `gem install --source`; the compact index for a package lists every version in publish order; a yank removes the version from the index while keeping the gem downloadable by exact URL per the documented rule.

### W6.10 Composer, Helm, and generic registry
- **Deliverable:** Composer repository (`packages.json`, per-package metadata, dist zip download) driven by a repository that stores `composer.json` metadata. Helm chart repository (`index.yaml`, chart `.tgz` push and fetch) over the generic transport. A generic registry exposing versioned artifact upload and download by name and version for ecosystems without a first-class adapter.
- **Depends on:** W6.4; W4.11.
- **Acceptance:** Integration tests resolve a Composer package with `composer require` and a Helm chart with `helm pull`; the generic registry upload and download round-trip preserves bytes and digest, and the same package name in two registry kinds does not collide.

### W6.11 Package permissions, visibility, and events
- **Deliverable:** Package visibility inherited from its owning repository or organization with no separate visibility switch, plus per-package granular access (read, write, admin, delete) granted to users, teams, and installation tokens. All decisions use the W4.11 permission registry extended with package resources. Domain events `package_published`, `package_version_deleted`, and `package_visibility_changed` join the W4.12 catalogue.
- **Depends on:** W4.11, W4.12; W6.4.
- **Acceptance:** Denied-and-allowed tests exist for pull, push, delete, and listing on public and private packages for user, team, deploy key, and installation-token principals; a package in a private repository never appears in another user's package list or in the API catalogue.

### W6.12 Retention, cleanup, quotas, and storage layout
- **Deliverable:** Owner-configurable retention rules (keep the newest N versions, keep versions newer than D days, keep versions matching a tag or version pattern, always keep release-referenced versions), evaluated by a scheduled cleanup job that produces a dry-run report before deleting. Quota accounting with a per-organization and per-repository byte limit, enforced under a transaction lock before finalizing an upload so two concurrent uploads cannot exceed the limit. Usage is exposed per owner and per registry in the UI and the API. Blob storage layout is documented: `<root>/packages/<registry>/<owner-id>/<package-id>/blobs/<sha256>` with version records referencing digests only, so a GC pass is safe to run online.
- **Depends on:** W6.4; `internal/jobs`.
- **Acceptance:** Tests cover a dry-run reporting exactly what a real run deletes, release-referenced versions surviving every rule, two concurrent uploads near the quota leaving usage at or below the limit, and GC never deleting a blob still referenced by a live version. A restore drill confirms package blobs and metadata survive a documented backup and restore.

## Data and interfaces

Registry coverage, with the native client each workstream must pass against a clean cache or daemon:

| Registry | Endpoint family | Authentication | Acceptance client |
| --- | --- | --- | --- |
| OCI / Docker | `/v2/` blobs, manifests, tags | Bearer token via `WWW-Authenticate` challenge | `docker` and `podman` |
| npm | `/api/packages/npm/` packument, tarball, publish | Bearer token | `npm publish` / `npm install` |
| Maven / Gradle | `/api/packages/maven/` artifact paths, `maven-metadata.xml` | Basic credentials | `mvn deploy`, Gradle `maven` repository |
| NuGet | `/api/packages/nuget/v3/index.json` and flat container | Basic credentials | `dotnet restore`, `dotnet nuget push` |
| RubyGems | `/api/packages/rubygems/` push, yank, compact index | Bearer token | `gem push`, `gem install --source` |
| Composer | `/api/packages/composer/` `packages.json`, dist zips | Basic credentials or token | `composer require` |
| Helm | `/api/packages/helm/` `index.yaml`, chart `.tgz` | Basic credentials or token | `helm pull`, `helm push` (OCI path uses the container registry) |
| Generic | `/api/packages/generic/<name>/<version>/<file>` | Bearer token | `curl` round trip |

Every registry kind resolves its caller to the same principal as the API and Git transports, so a token that lacks package write is refused identically on all of them.

Schema additions, all forward-only migrations in `migrations/`:

- `releases` (repository id, tag name, commit oid, title, body, author id, draft, prerelease, published at, created at, updated at).
- `release_assets` (release id, name, size, content type, storage key, download count, created by).
- `packages` (registry kind, owner type, owner id, name, visibility source, created at), unique on `(registry_kind, owner_type, owner_id, name)`.
- `package_versions` (package id, version, metadata JSON, published by, published at, yanked at, download count).
- `package_blobs` (digest, size, created at), `package_version_blobs` (version id, digest, role), with reference counts derived by query rather than stored counters.
- `package_permissions` (package id, principal type, principal id, level).
- `package_retention_rules` (owner type, owner id, rule kind, parameters) and `package_quota` (owner type, owner id, byte limit, bytes used as a cached value with a reconciliation job).
- `package_usage_events` for audit and chargeback-style reporting.

Packages touched: new `internal/packages` (registry interface, blob store interface, retention, quota, cleanup); `internal/repository` (releases, release assets); `internal/jobs` (index regeneration, cleanup, quota reconciliation); `internal/activity` (audit entries for publish, delete, retention run, quota change); `internal/webhooks` (package events); `internal/transport/http/web` (release pages, package browsing, retention and quota settings); `internal/transport/http/api` (releases, assets, package REST surface); `internal/platform/storage` (local blob backend, later an S3-compatible backend); `internal/transport/git` (tag creation for releases goes through the same runner).

API surface: release and asset REST endpoints; `/v2/` for OCI; `/api/packages/npm/...`, `/api/packages/maven/...`, `/api/packages/nuget/...`, `/api/packages/rubygems/...`, `/api/packages/composer/...`, `/api/packages/helm/...`, `/api/packages/generic/...`. All registry endpoints require the W4.8 credential model; none accepts an anonymous write.

Events: `release` (published, edited, deleted, prerelease toggled), `package_published`, `package_version_deleted`, `package_visibility_changed`.

Operator-visible state: bytes used per owner and per registry, object count, quota headroom, cleanup job outcome with the rule that triggered each deletion, blob scrub results, and upload rejection counts by reason. No metric carries a package name, a version string, or a token.

CLI and configuration: the storage root, per-asset and per-repository size caps, retention defaults, and quota defaults are instance configuration; a `gitnest packages gc --dry-run` command reports what a real run would delete using the same code path as the scheduled job.

## Security notes

- Registry writes are authenticated on every ecosystem; the token-auth paths (`WWW-Authenticate` challenge, basic credentials for Maven and NuGet, bearer for OCI) all resolve to the same principal and permission decision.
- Every upload is size-limited before it reaches final storage; uploads write to a temporary directory inside the configured storage root with a disk-space check, and are removed on failure or timeout.
- Manifest, metadata, and index files are parsed as untrusted input with bounded size and depth; no archive is extracted outside a temporary directory, and archive expansion is capped.
- Package names, versions, and paths supplied by clients are validated against per-registry grammar and never concatenated into a filesystem path; storage keys are derived from internal IDs and digests.
- Blob digests are verified on upload before a version record is created, and verified again on a scheduled scrub; a mismatch quarantines the blob and reports it.
- Package content is never executed by GitNest. Nothing in this phase runs a build, an install script, or a lifecycle hook from an uploaded artifact.
- Quota and retention deletions are audited with the rule that triggered them and the actor (user or scheduled job).
- Range downloads and redirects (if a future object-storage backend issues them) must not allow a caller to obtain a URL for a package they cannot read.

## Out of scope

- GitHub Container Registry-style analytics products, marketplace distribution, and paid private-registry plans.
- Mirroring, proxying, or caching of external registries.
- Dependency graph, dependency review, Dependabot, and any vulnerability or license scanning of published packages.
- Signing infrastructure beyond storing client-provided signatures and attestations as opaque blobs; no key management or provenance service is built here.
- Running builds to produce packages; Phase 5 workflows publish artifacts and this phase only receives them.
- Anonymous publish, cross-instance replication, and package federation.

## Exit criteria

- An operator can cut a release with generated notes and attached assets, and a user with read access can download an asset.
- `docker push` and `docker pull` work against GitNest for a private package with credentials, and are denied without them.
- Each implemented registry passes an integration test using its native client against a clean cache or daemon.
- Retention policies run as a dry run and as a real run with identical reported results, and no live version's blobs are removed.
- Quotas are enforced under concurrent upload, and usage reported in the UI and the API matches stored bytes.
- Package and release data survive a documented backup and restore, with the storage layout documented for operators.
