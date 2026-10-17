# Deployment

The conventional deployment is one host, one binary, one unit per run mode, PostgreSQL
on the same host or reachable by DSN, and a data directory outside the repository tree.

| File | What it is |
| --- | --- |
| `gitnest.service` | the web process: HTTP, the API, Git over HTTP, webhooks, LFS |
| `gitnest.env.example` | the startup environment, to be installed as `/etc/gitnest/gitnest.env` |
| `Dockerfile` | the container image, whose entrypoint selects the run mode |

**Two units the packaging document names are deliberately not here yet**, because their
run modes do not exist in this release and a unit for a command that cannot start is
worse than no unit at all — it would fail on a machine whose operator followed this
document:

- `gitnest-worker.service`. There is no background-job queue until the phase that
  introduces one. When it arrives, the unit is a copy of `gitnest.service` with
  `ExecStart=/usr/local/bin/gitnest worker` and a longer `TimeoutStopSec`, because a
  worker drains jobs rather than requests.
- `gitnest-ssh.service`. Git over SSH is a phase-2 transport. Until then, Git is served
  over HTTPS only, which is what this unit and the README document.

The `worker` and `ssh` run modes are also named in
[../docs/architecture/packaging.md](../docs/architecture/packaging.md) section 7, which
describes the target design rather than this release.

## Installing

```sh
# A dedicated account, with a home it owns and nothing else.
sudo useradd --system --create-home --home-dir /var/lib/gitnest --shell /usr/sbin/nologin gitnest

sudo install -m 0755 gitnest /usr/local/bin/gitnest
sudo install -d -o root -g gitnest -m 0750 /etc/gitnest
sudo install -o root -g gitnest -m 0640 deploy/gitnest.env.example /etc/gitnest/gitnest.env
sudo install -m 0644 deploy/gitnest.service /etc/systemd/system/
sudo systemctl daemon-reload

# Read /etc/gitnest/gitnest.env and replace every placeholder before continuing.
sudo -e /etc/gitnest/gitnest.env

# The database has to exist and be migrated before the service starts.
sudo -u gitnest gitnest migrate

# The first administrator, on an empty instance. The password is prompted for and
# never passed as an argument, because arguments are visible in the process table.
sudo -u gitnest gitnest admin create --username admin --email admin@example.com

sudo systemctl enable --now gitnest.service
```

## The units

**`Type=notify` means what it says.** The binary reports `READY=1` to systemd only
after it has confirmed the database is reachable — not when the listener opens. A
process that is listening but cannot reach its database is therefore never reported
ready, so a supervisor does not send it traffic. If the notification never arrives,
systemd times the unit out and reports it failed, which is the correct outcome: the
instance could not serve. Check `journalctl -u gitnest` for the startup line.

The units carry the hardening the packaging document specifies. Two settings are worth
understanding rather than copying:

- `ReadWritePaths=/var/lib/gitnest` with `ProtectSystem=strict` makes the whole
  filesystem read-only to the process except the data directory. A compromise cannot
  write a binary or a unit file.
- `CapabilityBoundingSet=` (empty) means the process holds no capability at all. It
  binds 8080 and 2222, both above 1024, so it needs none. **If you change the port to
  something below 1024, the unit will fail to bind**, and adding `CAP_NET_BIND_SERVICE`
  is a deliberate decision to make rather than an oversight to fix.

## The environment file

`/etc/gitnest/gitnest.env` holds startup configuration only: the public URL, the
database DSN, the data directory, the bind addresses, the log level, the shutdown and
header timeouts, and the connection-pool budget.

It holds **no secret values that can be avoided and none that are committed**. The
database password is the one value it must contain, which is why the file is mode
`0640`, owned by root with the service account as its group, and never in the
repository and never in an image layer. An operator who would rather keep even that out
of the file can use a root-owned file referenced by `PGPASSFILE`, which the database
client programs read.

Instance policy does **not** live here. Registration mode, default visibility, quotas,
SMTP, and the search backend are instance settings, changed through the operator
console without a restart. The split between the two layers is defined in
[../docs/architecture/configuration.md](../docs/architecture/configuration.md), and an
upgrade never moves a setting from one layer to the other silently.

## Upgrading

The procedure is in
[../docs/architecture/packaging.md](../docs/architecture/packaging.md) section 11. In
short, and in this order:

```sh
# 1. Read the release notes for migrations and whether any is irreversible.
# 2. Verify the artifacts against SHA256SUMS.
# 3. Take and verify a recovery set. An upgrade without one is not supported.
gitnest backup --target /srv/backups/gitnest-$(date +%F)
gitnest backup-verify --source /srv/backups/gitnest-$(date +%F)

# 4. Preflight. Read-only, safe against production, and it exits non-zero when the
#    database is ahead of the binary — which means a rollback happened to the binary
#    but not to the schema.
gitnest migrate --dry-run

# 5. Apply, with the lock, once, with the operator present.
gitnest migrate

# 6. Upgrade the units in order. This release has one unit; a deployment with a
#    worker stops it first, upgrades it first, and starts it last.
sudo systemctl stop gitnest.service
sudo install -m 0755 gitnest /usr/local/bin/gitnest
sudo systemctl start gitnest.service

# 7. Verify.
curl -fsS https://gitnest.example.com/api/ready
git clone https://gitnest.example.com/<owner>/<repo>.git /tmp/verify-clone
```

**A migration never runs from a unit's `ExecStartPre`.** The preflight is read-only and
is safe there; the migration itself is not, because it needs the operator present and a
verified backup behind it.