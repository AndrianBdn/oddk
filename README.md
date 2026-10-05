# ODDK — Opinionated Database Deployment Kit

<a href="https://github.com/andrianbdn/oddk/releases/latest"><img src="https://img.shields.io/github/v/release/andrianbdn/oddk" /></a>
<a href="./LICENSE"><img src="https://img.shields.io/github/license/andrianbdn/oddk" /></a>
<a href="./go.mod"><img src="https://img.shields.io/github/go-mod/go-version/andrianbdn/oddk" /></a>
[![Go Report Card](https://goreportcard.com/badge/github.com/andrianbdn/oddk)](https://goreportcard.com/report/github.com/andrianbdn/oddk)

**Run PostgreSQL on your own Linux box with the ergonomics of a managed service.**

ODDK is a single Go binary that manages PostgreSQL the way a cloud provider's
managed database does — create an instance, get a connection string, take
scheduled snapshots, ship them offsite to S3, watch health, restore on demand,
upgrade major versions — except it all runs locally against Docker, on hardware
you control. Think "a small, self-hosted RDS for Postgres."

```bash
oddk create --name app --version 17 --port 5432 --cpu 4 --ram 8   # pulls the image if needed
oddk instance get-postgres-password app --conn
# postgresql://postgres:••••••••@10.88.0.1:5432/postgres
```

---

## What it is

- **A local "managed Postgres" control plane.** One daemon + CLI that owns the
  full lifecycle of PostgreSQL instances running as Docker containers.
- **Opinionated and batteries-included.** Sensible defaults for resources,
  shared memory, networking, and tuning — plus AWS-style *parameter groups* when
  you want to override them.
- **Operationally complete.** Whole-deployment *snapshots* — scheduled, shipped
  to S3, and able to rebuild a single instance or an entire host — plus health
  monitoring with Email/Slack/Telegram/Webhook alerts, password and user
  management, minor-version image switches, dump/restore major upgrades, and
  per-instance backups (deprecated; removed after 2026-12-31).
- **Actually recoverable.** A snapshot carries every instance's data *and* ODDK's
  own configuration, so a dead host can be rebuilt from one archive plus the
  master key — not reassembled by hand from per-database dumps.
- **Secure by default for a single host.** Secrets encrypted at rest, a
  loopback-only API behind a bearer token, and Postgres bound to a host-local
  bridge — not the public internet.
- **Single binary, no runtime dependencies** beyond Docker. Pure Go, builds
  static, installs in seconds.

## What it is *not*

- **Not a high-availability / clustering / replication manager.** No failover,
  no streaming replicas, no quorum. It runs standalone instances well.
- **Not a multi-tenant hosted service.** It assumes a *single trusted operator*
  on a *single host*. Anyone with the API token has admin-equivalent control.
- **Not an internet-facing database gateway.** The API binds to `127.0.0.1` and
  Postgres binds to a host-local Docker bridge. Reach them over an SSH tunnel,
  not by exposing ports.
- **Not a Postgres fork, driver, or connection pooler.** It orchestrates the
  *official* PostgreSQL images (and compatible ones like `pgvector`/`postgis`);
  it doesn't replace your client library or PgBouncer.
- **Not a Kubernetes operator.** It talks to the Docker API directly. If you're
  on Kubernetes, use an operator instead.
- **Not something you run *inside* Docker.** ODDK manages and monitors Docker
  from the host — it is the control plane, not a workload. See
  [Run ODDK on the host, not inside a container](#run-oddk-on-the-host-not-inside-a-container).
- **Not for Windows or production macOS.** Linux is the deployment target;
  macOS is supported for development only.

## Why ODDK

If you've ever wanted RDS-style convenience — "give me a database, back it up,
tell me when it's unhealthy, let me restore it" — without the cloud bill, the
network exposure, or hand-rolling `docker run` + `pg_dump` + cron + a monitoring
script, ODDK is that, as one tool with one mental model.

| You want… | ODDK gives you… |
|---|---|
| A new database, fast | `oddk create` → ready-to-use Postgres with a connection string |
| Confidence it's backed up | `snapshot make`, one scheduled snapshot covering everything, S3 offsite with retention |
| To not lose data | `snapshot restore-instance` (one instance) or `snapshot apply` (a whole host) |
| To know when it breaks | Health checks + degraded/restored notifications |
| To tune Postgres safely | AWS-style parameter groups with expression evaluation |
| To move to a new major | `instance major-upgrade` via dump/restore |
| Secrets handled properly | AES-256-GCM-encrypted passwords, tokenized API auth |

---

## Requirements

- **Linux** (x86_64 or arm64)
- **Docker** (running)
- **systemd** (for the installed service)

---

## Run ODDK on the host, not inside a container

**ODDK is a Docker control plane. Run it on the host, directly on the machine
that runs Docker — never inside a container.**

The whole point of ODDK is to *manage and monitor* Docker: it creates and
destroys PostgreSQL containers, attaches them to a host bridge network, reads
host disk/CPU/memory for health checks, and writes state and snapshot/backup
archives to host paths. That is the opposite of being a containerized workload itself. Running
ODDK inside Docker inverts the relationship and breaks its assumptions —
host-level resource metrics, the `10.88.0.0/16` bridge and `10.88.0.1` gateway
binding, data/backup paths, and the systemd service lifecycle all expect a host
process. Bind-mounting the Docker socket into a container to work around this is
exactly the inversion ODDK is designed to avoid, and is not supported.

If what you actually want is to run a database *inside* Docker/Compose as part
of a containerized stack, that is a different problem with different tools — use
Docker Compose, a Kubernetes operator, or your platform's managed database
instead. ODDK is for owning the host and treating Docker as the thing it drives.

---

## Installation

On a Linux server with Docker and systemd, install (or update) the latest
release:

```bash
curl -fsSL https://raw.githubusercontent.com/andrianbdn/oddk/main/install.sh | sh
```

Pin a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/andrianbdn/oddk/main/install.sh | sh -s -- --version v0.1.39
```

The installer downloads the release binary from GitHub, verifies it against the
published `SHA256SUMS`, and:

- installs the binary to `/usr/local/bin/oddk`
- creates a dedicated `oddk` service user (no login shell) with state under
  `/var/lib/oddk` (`data/`, `backups/`)
- installs and starts a systemd unit (`oddk.service`)
- configures the CLI for the user who ran the installer, writing
  `~/.config/oddk/cli.json`

That last step means the person who runs the installer can use `oddk` right
away — no `sudo`, no becoming the `oddk` user.

**Installing and updating use the same command.** Re-run the curl installer at
any time — on an existing install it detects the service, swaps the binary in
place, restarts, and keeps the previous binary as `oddk.prev` for instant
rollback. There is no separate update step.

Before updating, skim [CHANGELOG.md](./CHANGELOG.md). Most releases need nothing
from you, but the entries marked **Action** do — a credential to rotate, or a
schedule to resume after a disaster-recovery restore.

### Configuring the CLI for another user

The CLI authenticates to the daemon with a bearer token. To set up `oddk` for an
additional user, mint a token and install their config in one step:

```bash
eval "$(sudo -u oddk /usr/local/bin/oddk auth mint)"
```

> The plaintext token is shown only when created and cannot be read back later.
> If you lose it, mint a new one with `oddk auth mint`. Use `oddk auth mint --json`
> to print the config instead of eval-able shell, `oddk auth list` to see existing
> tokens, and `oddk auth delete <id>` to revoke one.

---

## First steps

After installation the daemon is running and your CLI is configured. From here:

```bash
# 1. Create an instance — 4 CPUs, 8 GB RAM, listening on port 5432.
#    The PostgreSQL image is pulled automatically if it isn't already local.
oddk create --name app --version 17 --port 5432 --cpu 4 --ram 8

# 2. See what you have
oddk list

# 3. Get connection details (password is auto-generated, encrypted at rest)
oddk instance get-postgres-password app --conn        # full connection string
eval "$(oddk instance get-postgres-password app --envs)"  # export PG* env vars

# 4. Open a psql shell
oddk instance psql app
```

**Connecting from the host:**

```
postgresql://postgres:PASSWORD@10.88.0.1:<port>/postgres
```

**Connecting from another Docker container** (e.g. your app's `docker-compose.yml`):

```yaml
extra_hosts:
  - "host.docker.internal:host-gateway"
# then: postgresql://postgres:PASSWORD@host.docker.internal:<port>/postgres
```

---

## Common usage

`oddk` is organized into subcommands. Everything below has `--help`
(`oddk instance --help`, `oddk snapshot --help`, …).

### Instances

```bash
oddk create --name app --version 17 --port 5432 --cpu 4 --ram 8
oddk create --name dev --version 17 --port 5433 --cpu 1 --ram 1024M   # RAM accepts M/MB/MiB
oddk instance status app
oddk instance start app
oddk instance stop app
oddk instance logs app --follow
oddk instance destroy app

oddk list             # all instances at a glance
oddk checklist        # audit overview, one detailed block per instance: health,
                      # parameter group, and snapshot coverage (is this instance's
                      # data in the newest snapshot?); plus global snapshot and
                      # notification status
oddk checklist --json # same data as JSON
```

Create/start/switch/reconfigure **block until Postgres actually accepts
connections** before reporting success, so a command never returns "running"
while the server is still coming up.

### Databases & users

Deploying a new service? One command creates the database **and** its owner
user (rolling back the user if database creation fails), and prints the
generated password and a ready-to-paste connection string. The user owns the
database, so migrations just work:

```bash
oddk instance create-db app --database billing --username billing   # DB + owner user
```

The pieces are also available separately (read-only and extra users are added
with `add-db-user` once the database exists):

```bash
oddk instance create-db app --database analytics
oddk instance list-dbs app

oddk instance add-db-user app --username appuser --database analytics            # read-write
oddk instance add-db-user app --username reader  --database analytics --readonly # read-only
oddk instance add-db-user app --username appuser --database analytics --owner    # owner (runs migrations)
oddk instance reset-db-user-password app --username appuser
oddk instance delete-db-user app --username appuser
```

`add-db-user` creates the role, grants access, and applies any requested ownership
changes in one transaction. A failed grant or ownership transfer rolls all of them
back. Ownership transfer includes public-schema functions and stored procedures.

`delete-db-user` first reassigns everything the user owns to `postgres`, then
revokes its grants and drops the role. If that reassignment fails the command
aborts and leaves the user in place — it will not proceed to a step that would
drop the objects instead of the grants. On a very large database the reassign
can exhaust the shared lock table (`out of shared memory ... increase
max_locks_per_transaction`); raise `max_locks_per_transaction` with a parameter
group and retry.

Concurrent application DDL is also guarded: if a new object would be deleted
during privilege cleanup, the command aborts and rolls back that database's
cleanup, preserving the object and the user. Stop the application's DDL and
retry. Cleanup already completed in other databases is not rolled back.

### Passwords

```bash
oddk instance get-postgres-password app                 # structured details
oddk instance get-postgres-password app --plain         # just the password
oddk instance get-postgres-password app --conn          # connection string
NEW_PGPASSWORD=secret oddk instance set-postgres-password app
```

### Snapshots — the recommended way to protect a deployment

A **snapshot** captures *everything*: every instance's databases and roles, plus
ODDK's own configuration, in one archive. It is what a host migration or a real
disaster recovery restores from. A per-instance backup cannot do that — it holds
one instance's data and none of the configuration needed to rebuild it.

```bash
# Capture the whole deployment (physical/binary by default — see below)
oddk snapshot make --comment "before major upgrade"
oddk snapshot make --logical            # portable pg_dump-based format
oddk snapshot list

# Suspend a schedule without losing it (and put it back)
oddk snapshot setup-cron --pause
oddk snapshot setup-cron --resume

# Schedule it. One schedule per deployment — a snapshot covers every instance.
oddk snapshot setup-cron --utc-hour 3                     # daily at 03:00 UTC
oddk snapshot setup-cron --utc-hour 3 --interval-hours 6  # 03,09,15,21 UTC
oddk snapshot setup-cron --utc-hour 3 --logical           # schedule portable snapshots
oddk snapshot list-cron

# Already scheduling per-instance backups? Move those schedules over in one step.
oddk snapshot migrate-from-backups --dry-run   # preview; changes nothing
oddk snapshot migrate-from-backups --yes

# Offsite (requires `oddk offsite apply`, below)
oddk snapshot upload <id>
oddk snapshot download <id>
oddk snapshot remove-local <id>
oddk snapshot remove-remote <id>

# Did last night's scheduled work actually run?
oddk cron logs                # last 20 runs: backups and snapshots, one row each
oddk cron logs --failures     # only runs with a failed phase
oddk cron logs --instance app
```

**One instance failing does not cost the others their archive.** If an instance
cannot be captured — its server is unreachable, its stored password no longer
authenticates, PostgreSQL is out of WAL senders, its container has vanished — that
instance is recorded in the snapshot as configuration-only and everything else is
captured normally. The
archive is kept, uploaded and catalogued, because it is still the newest restore
point for every other instance. But the run *reports failure*: `snapshot make`
exits non-zero and names the instance, a scheduled run's capture phase is marked
failed and notifies, and `oddk checklist` shows that instance as `✗ config-only`
rather than covered. An archive that is empty for somebody must never read as
protection.

Restoring comes in four shapes:

```bash
# 1. Rebuild ONE instance into a deployment that stays up, from a local file.
#    Creates it if it is gone; replaces its data if it is still there.
oddk snapshot restore-instance --instance app --file snapshot-db01-20260729140312.tar.zst

# 2. The same, straight from S3 — no manual download step.
oddk snapshot restore-instance --instance app --id 7   # this host's catalogue; downloads
                                                       # from S3 if the local copy is gone
oddk snapshot restore-instance --instance app \
      --s3-uri 's3://bucket/oddk-backups/*snapshots*/2026-07-29/snapshot-db01-20260729140312.tar.zst' \
      --master-key /mnt/restore/master.key             # another deployment's snapshot
# Find URIs with `oddk snapshot list-remote` — it lists what is actually in the
# bucket, including snapshots whose records died with another host.

# 3. Restore ONE DATABASE into a running instance, as a new database. The
#    instance, its other databases, roles and password are left alone, and the
#    original name must be free, so --restore-as puts it next to the live one.
#    Takes the same --file / --id / --s3-uri sources as restore-instance.
oddk snapshot restore-database --instance app --database sales --id 7 \
      --restore-as sales_yesterday
#    --from-instance reads another instance's copy: prod's database into staging.
oddk snapshot restore-database --instance staging --from-instance prod \
      --database sales --id 7

# 4. Rebuild a WHOLE HOST — migration or disaster recovery.
#    Runs locally, not through the daemon, so it works when the daemon cannot start.
systemctl stop oddk
sudo -u oddk oddk snapshot apply \
      --file /mnt/restore/snapshot-db01-20260729140312.tar.zst \
      --master-key /mnt/restore/master.key
systemctl start oddk

# apply PAUSES every restored schedule. Resume once this host owns its bucket:
oddk snapshot setup-cron --resume

# apply can also fetch the archive itself, using this shell's AWS credentials —
# see "Disaster recovery from S3" below for the full walkthrough.
sudo -u oddk oddk snapshot apply \
      --s3-uri 's3://bucket/oddk-backups/*snapshots*/2026-07-29/snapshot-db01-20260729140312.tar.zst' \
      --master-key /mnt/restore/master.key
```

What you need to know:

- **Snapshots are physical (binary) by default.** Each running instance is
  captured with `pg_basebackup` — fast, gentle on a busy server, and
  byte-for-byte faithful (per-database settings, database-level privileges and
  ICU collations all survive, which the logical format cannot promise). A
  physical snapshot restores onto the same PostgreSQL major and the same CPU
  architecture; `--logical` produces the portable `pg_dump`-based format for
  cross-architecture moves and for UNLOGGED table rows that a physical restore
  would empty. To restore a single database, use `oddk snapshot
  restore-database` (above).
- **A single-database restore from a physical snapshot starts a scratch copy
  of the instance.** A physical archive has no per-database dumps, so
  `restore-database` starts the instance's copy as a throwaway cluster, dumps
  the one database out of it, and removes it. The copy has **no network
  access**, and its logical-replication workers and WAL archiving are off. A
  copy of a production cluster brings its subscriptions, foreign servers and
  scheduled jobs with it, and must not be able to act on the systems they
  point at. It needs free disk for the whole cluster while it runs. As with
  `backup restore`, the restored objects are owned by `postgres` and carry no
  object privileges. Re-grant what your application needs, for example with
  `oddk instance add-db-user ... --owner`.
- **UNLOGGED tables come back empty from a physical restore.** This is standard
  physical-backup semantics (RDS storage snapshots behave the same): unlogged
  tables are truncated by any crash recovery, which is what a physical restore
  performs — the trade you accept for their WAL-free write speed. If an
  unlogged table's contents must survive a restore, either make it a normal
  table or use `--logical`, which dumps its rows.
- **Back up `master.key` separately.** It is deliberately *not* in the archive,
  and a snapshot cannot be applied without it.
- **Snapshots are not encrypted, and they carry more than your data.** The
  archive holds database contents and role password hashes in plaintext, and it
  embeds `oddk.db` verbatim. The master key encrypts three columns in there —
  each instance's postgres password, the S3 secret access key, and (since
  0.1.80) every notification config. Everything else in that database is in the
  clear. So whoever can read a snapshot can read your databases, and that
  includes anyone with read access to the offsite bucket you upload to; restrict
  it accordingly.
  **Archives written before 0.1.80 also carry your notification credentials in
  the clear** — the SMTP password, the Slack webhook URL, the Telegram bot
  token, and any `Authorization` header on a webhook. Encrypting the column
  going forward does not unpublish what already shipped: if an older archive
  has been anywhere you would not put a password, rotate those credentials.
- **What gets captured is decided by the container, not by recorded state.** A
  snapshot asks Docker what each container is actually doing at capture time, so
  an instance whose recorded status has drifted is still captured correctly. A
  stopped container is captured as a cold copy of its data directory (and
  restored back to a stopped instance); only with `--logical` — which needs a
  live server to dump — is it reduced to configuration-only. When an instance
  *should* have been capturable but was not — its container has vanished, its
  state cannot be read, or it is paused or restarting — the run is reported as
  failed, because the data is still on this host (ODDK keeps it in a Docker
  volume that removing a container does not delete) and the archive was written
  without it. Only the cases where there is nothing to read fall back to a quiet
  configuration-only entry: an instance with no container at all, or one you
  deliberately stopped under `--logical`. Either way the specific reason is
  recorded in the manifest and printed on stdout — reported, never silent.
- **Every archive's SHA-256 is recorded when it is written, and downloads are
  checked against it.** `snapshot list --json` shows it as `sha256`, the value
  `sha256sum` prints, so any copy can be checked by hand. `snapshot download`,
  `backup download` and `restore-instance --id` refuse a copy whose digest
  differs, even when the archive is otherwise intact. That catches an object
  replaced in the bucket, which would otherwise restore somebody else's data.
- Restoring an instance sets its postgres password to the snapshot's, because the
  archive carries only the hash. Re-read it with `instance get-postgres-password`.
- Retention keeps the newest snapshots regardless of age, so a run of failed
  captures can never expire everything you have.
- Backups and snapshots larger than 5 GiB upload automatically using S3
  multipart uploads. Failed uploads keep their local copy for retry. Allow
  `s3:AbortMultipartUpload` on the archive prefix so failed or canceled uploads
  can clean up their uploaded parts. Configure a bucket lifecycle rule to abort
  incomplete multipart uploads left by a daemon crash or loss of connectivity.
  For SSE-KMS buckets, the upload identity needs `kms:Decrypt` and
  `kms:GenerateDataKey` on the encryption key.

`oddk checklist` reports whether snapshots are scheduled, how stale the newest one
is, and — per instance — whether that instance's data is actually in the newest
snapshot: an instance captured configuration-only (e.g. stopped during a logical
capture) is flagged rather than counted as protected, and an instance created
after the newest snapshot reads "not yet captured" until the next run. Per-instance
backups are legacy and no longer appear in the audit, except as a warning when an
instance still has an un-migrated backup schedule.

#### Moving an existing deployment onto snapshots

`oddk snapshot migrate-from-backups` turns your per-instance backup schedules
into the single deployment-wide snapshot schedule and then removes them. It picks
the most common hour and the **longest** retention window any schedule used, so
nothing is silently shortened; `--utc-hour`, `--interval-hours` and the two
`--cleanup-*-days` flags override the derived values. Offsite settings are global
and already shared by both paths, so there is nothing there to move.

Snapshots are scheduled *before* the backup schedules are removed, so an
interrupted run leaves both active rather than neither. Re-running on an
already-migrated host is a quiet success, and an existing snapshot schedule is
kept rather than overwritten unless you pass an override flag — both make it safe
to run across a fleet. Add `--dry-run` to preview, `--yes` to skip the prompt, and
`--json` (with either) for scripted rollouts.

> **Your existing backups are kept, but they stop being pruned.** Age-based
> cleanup only ever runs from a backup schedule, so removing the schedule ends it
> permanently. The command reports how many archives and how much disk this
> leaves behind. They stay restorable with `oddk backup restore`; remove them
> with `oddk backup remove-local` once you trust the snapshot schedule — or all
> at once with `oddk backup dangerously-drop-all` (below).

This is a transitional command. Per-instance `backup` itself is unaffected.
Restoring or cloning a **single database** no longer needs it: `oddk snapshot
restore-database` does that from a snapshot.

#### Disaster recovery from S3

A dead host's snapshots live in the bucket; the replacement host starts with
nothing — no ODDK state, no CLI token, no AWS tooling. `snapshot list-remote`
and `snapshot apply --s3-uri` are built for exactly that: both run without a
daemon and without a token, using the plain AWS credentials of the shell they
run in, so you never have to install and configure a second S3 tool mid-outage.

**1. Install ODDK** on the new host (the same curl installer as above). It
starts a fresh empty daemon — that's fine, `apply` handles the fresh-install
collision itself.

**2. Get AWS credentials to the `oddk` user**, who runs the apply. Three
options, best first:

- **EC2 instance role** — zero configuration. If the host's role can read the
  bucket, `sudo -u oddk oddk snapshot apply ...` just works.
- **Environment variables** — `sudo` strips `AWS_*` from the environment, so
  pass them through explicitly (never put secrets on the command line itself —
  they would be visible in the process list):

  ```bash
  sudo --preserve-env=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_SESSION_TOKEN,AWS_REGION \
       -u oddk oddk snapshot apply --s3-uri '...' --master-key /mnt/restore/master.key
  ```

- **A credentials file for the `oddk` user** — write a standard
  `[default]` credentials file with your editor, install it, and destroy both
  copies when done:

  ```bash
  sudo install -d -o oddk -m 700 ~oddk/.aws
  sudo install -o oddk -m 600 /tmp/creds ~oddk/.aws/credentials && shred -u /tmp/creds
  # ... run the restore ...
  sudo shred -u ~oddk/.aws/credentials
  ```

**3. Find the snapshot.** With an explicit URI, `list-remote` lists the bucket
directly — newest first, with ready-to-paste URIs:

```bash
oddk snapshot list-remote s3://my-backup-bucket/oddk-backups/
```

**4. Apply it, start, audit:**

```bash
systemctl stop oddk
sudo -u oddk oddk snapshot apply \
      --s3-uri 's3://my-backup-bucket/oddk-backups/*snapshots*/2026-07-29/snapshot-db01-20260729140312.tar.zst' \
      --master-key /mnt/restore/master.key
systemctl start oddk
oddk checklist
```

The archive is downloaded into a managed `downloads/` area under the backup
directory before anything is touched — a failed apply keeps it there for the
retry (re-running with the same `--s3-uri` reuses it), and it is pruned
automatically after 7 days.

> **`apply` pauses every restored schedule, and that is on purpose.** The
> archive carries the source host's `oddk.db`, which holds its snapshot
> schedule, its per-instance backup schedules *and* its offsite bucket and
> credentials. Starting the daemon unpaused would put two hosts on one bucket:
> this one would upload under the same layout and — the dangerous half — run
> offsite **retention** against it, deleting archives the *source* still
> catalogues. Since `apply` cannot know whether the source is alive, it fails
> safe.
>
> So a restored host takes **no snapshots until you resume**. That is reported
> three ways so it cannot be forgotten: `apply` prints it, `oddk checklist`
> shows `✗ ... PAUSED, NO SNAPSHOTS ARE BEING TAKEN`, and a notification
> repeats daily until it is resumed or removed.
>
> ```bash
> oddk snapshot setup-cron --resume                      # the snapshot schedule
> oddk backup setup-cron --instance <name> --resume      # any legacy backup schedule
> ```
>
> If this is a **rehearsal** and the source is still live, leave them paused —
> or point this host at its own bucket with `oddk offsite apply` first.
>
> If the source host is **gone** and this one replaces it, `--no-pause-schedules`
> skips the pause so the replacement is protected from its first scheduled run
> rather than from whenever somebody reads the checklist. Only use it when you
> know nothing else writes to that bucket — that is the one fact `apply` cannot
> check for you. It leaves the schedules as the archive carried them, so a plan
> the source had already paused stays paused.

> **You still need `master.key`, and it is deliberately *not* in the bucket** —
> an archive and its key stored together would defeat the encryption of the
> secrets inside. And remember that **snapshots themselves are not encrypted**:
> they hold database contents and role password hashes in plaintext, so guard
> bucket access accordingly. (Notification credentials are encrypted since
> 0.1.80; older archives carry them in the clear.)

### Offsite storage (S3)

One S3 configuration serves the whole deployment — snapshot uploads and legacy
backup uploads alike:

```bash
oddk offsite apply --file offsite.json   # see `oddk offsite get` for the template
oddk offsite test
```

The configuration JSON (`oddk offsite get` prints a template when none is
configured):

```json
{
  "type": "s3",
  "bucket": "my-backup-bucket",
  "region": "us-east-1",
  "accessKeyId": "YOUR_ACCESS_KEY_ID",
  "secretAccessKey": "YOUR_SECRET_ACCESS_KEY",
  "bucketPath": "oddk-backups/",
  "ec2IamRole": false
}
```

- **`type`** — only `"s3"` is supported.
- **`bucket`** — required.
- **`region` / `endpoint`** — at least one must be set, so requests are never
  signed for a guessed location. Set `endpoint` (an `http(s)` URL) for
  S3-compatible storage; it also switches the client to path-style addressing,
  which most compatible services require.
- **`accessKeyId` / `secretAccessKey`** — required unless `ec2IamRole` is
  `true`. The secret is encrypted at rest with the master key; on later
  `offsite get` calls it is shown as a placeholder, never echoed back.
- **`ec2IamRole`** — set `true` (and leave both keys empty) to authenticate
  with the host's EC2 instance role instead, so no long-lived secret exists on
  disk at all.
- **`bucketPath`** — optional key prefix. It must end with `/` (e.g.
  `oddk-backups/`), must not start with `/`, and may not contain `//` or
  `.`/`..` segments — so a malformed prefix can't scatter uploads across the
  bucket root. Empty means the bucket root.

When offsite is configured, each scheduled snapshot run uploads the new
snapshot, retries earlier failed uploads, and then applies retention — and
local retention never deletes an archive whose only copy is local.

Retention keeps the newest two snapshots regardless of age, plus the newest
**complete** one (every instance captured with data), so a run of degraded
captures cannot expire the last fully restorable archive. That extra pin is
bounded: it holds for your retention window plus 30 days, then releases with a
warning naming `oddk checklist`. A deployment with a permanently
configuration-only instance never produces another complete archive, and an
unbounded pin would keep that one on disk and in your bucket forever while the
checklist quietly reported the instance as uncovered.

The same two safeguards apply to scheduled **backups**: a local backup with no
remote copy is never aged out while offsite is configured, and retention (local
and offsite) always keeps the newest two regardless of age, so a job that has
been failing longer than its retention window cannot expire everything you have.

**Stored settings and ambient credentials are two separate paths.** Everything
the *daemon* does offsite — scheduled uploads, `snapshot upload`/`download`,
the zero-argument `snapshot list-remote`, and `restore-instance --id` — uses
the stored configuration above. The *daemon-less* commands
(`snapshot list-remote s3://...` and `snapshot apply --s3-uri`) instead use
whatever AWS credentials their shell has (env vars, an `~/.aws` profile, an EC2
instance role), because a disaster-recovery host has no stored settings yet —
they live inside the snapshot it is trying to restore.
`snapshot restore-instance --s3-uri` bridges the two: the CLI resolves your
shell's credentials and passes them along, and the daemon prefers its own
offsite settings whenever the bucket matches.

Archives fetched by URI land in a managed `downloads/` area under the backup
directory and are pruned after 7 days; everything there is re-fetchable from
S3, so deleting it never loses data.

### Deprecated: per-instance backups

> **Per-instance backups are deprecated and will be removed in the first
> release after 2026-12-31.** Snapshots do everything they did, including
> restoring or cloning a single database (`oddk snapshot restore-database`).
>
> Until then:
> - every `oddk backup` command prints a notice on stderr naming its
>   replacement (stdout and `--json` output are unchanged);
> - **new backup schedules are refused.** Existing schedules keep running and
>   can still be changed, paused, resumed or removed. Move them with
>   `oddk snapshot migrate-from-backups`;
> - restore, list, download and `dangerously-drop-all` keep working, so the
>   archives you already have stay usable.
>
> | Instead of | Use |
> |---|---|
> | `oddk backup make` | `oddk snapshot make` |
> | `oddk backup restore --database` | `oddk snapshot restore-database` |
> | `oddk backup setup-cron` | `oddk snapshot setup-cron` (or `migrate-from-backups`) |
> | `oddk backup list` / `list-cron` | `oddk snapshot list` / `list-cron` |
> | `oddk backup upload` / `download` / `remove-*` | the `oddk snapshot` commands of the same name |

```bash
oddk backup make app --comment "before deploy"
oddk backup list --instance app
oddk backup restore --instance app --id 42 --database analytics
oddk backup restore --instance app --id 42 --database analytics --restore-as analytics_copy
oddk backup restore --instance app --file /path/to/backup.tar.zst --database analytics

# Existing schedules (new ones are refused; use `oddk snapshot setup-cron`)
oddk backup setup-cron --instance app --cleanup-local-days 14   # keeps the existing hour
oddk backup setup-cron --instance app --pause                  # suspend without deleting
oddk backup setup-cron --instance app --resume
oddk backup list-cron                                          # STATUS column: active / PAUSED
oddk backup upload app <backup-id>
oddk backup download app <backup-id>
```

Fields you do not pass to `setup-cron` are preserved, matching
`oddk snapshot setup-cron` — changing a retention window cannot silently move
the hour.

Backups record roles with database-level `CREATE` access, and both restore and
`major-upgrade` reapply those grants automatically. A role must already exist on
the target instance to receive its grant; missing roles are reported and skipped
without failing the operation. Older archives without this metadata retain the
previous behavior.

Done with per-instance backups? Once the snapshot schedule has proven itself,
delete every leftover backup in one sweep — local archives, S3 copies, and the
whole backup history, including backups of instances that no longer exist.
Snapshots are untouched:

```bash
oddk backup dangerously-drop-all           # preview — changes nothing
oddk backup dangerously-drop-all --apply   # delete (asks for confirmation)
```

The preview warns if any instance still has a backup schedule (migrate it
first — the next cron run would just create new backups) and if no snapshot
exists yet, in which case these backups are the only thing a restore could use.

### Custom images (pgvector, postgis, …)

```bash
oddk create --name vec --version 17 --image pgvector/pgvector:pg17-trixie --port 5436 --cpu 2 --ram 4
```

### Updating, switching & major upgrades

```bash
# Pick up a patch/security release for the instance's current image tag
oddk instance update app

# Switch to a different image, same major version — fast, reuses the volume
oddk instance switch app --image pgvector/pgvector:pg17-trixie

# New major version — dump/restore migration (causes downtime; backs up first)
oddk instance major-upgrade app --target-version 18 --yes
```

> `create`, `switch`, `update` and `major-upgrade` pull the image automatically
> when needed — `oddk pull` is optional, for pre-warming or CI. Quiesce writes
> before a major upgrade; changes made after it starts are not migrated.
> Cross-major `switch` is rejected up front — use `major-upgrade`.

### Parameter groups (AWS-style tuning)

```bash
oddk parameters get                                   # list groups
oddk parameters get --name default:2025-08-27         # inspect one
oddk parameters put custom --file params.json         # create/update
oddk create --name app --version 17 --port 5432 --cpu 4 --ram 8 --parameter-group custom
oddk instance apply app --parameter-group custom      # reconfigure in place
oddk instance apply app --cpu 8 --ram 16              # resize in place (data kept)
oddk instance apply app --port 5433                   # move the port
```

`instance apply` changes any combination of parameter group, CPU, RAM and port.
Flags you do not pass keep their current value. The container is recreated (a
brief restart; the data volume is untouched), and because parameter groups
resolve against the instance's RAM, a resize re-derives `shared_buffers` and
friends and re-checks that they fit the new size.

Parameters support expression evaluation against the instance's resources, e.g.
`"{expr}DBContainerMemoryMB / 4{/expr} MB"` for `shared_buffers`.

**Getting a group — or a size — wrong does not cost you the instance.** `oddk
instance apply` — and `create`, `instance switch`, `instance update`, which
rebuild the container the same way — checks what it can *before* the running
container is touched: a missing image, a group that will not resolve, one whose
shared-memory arena cannot fit the instance's RAM, a port another instance
holds, or more CPU or RAM than the host has is refused outright and the old
container keeps serving. If the new container is built but PostgreSQL never
comes up, ODDK stops it, puts the previous configuration back, and reports the
failure — the instance stays up. The stored group changes only once PostgreSQL
is accepting connections, so a failed apply can be retried with the *same*
group instead of forcing you to invent a different one.

The shared-memory check is deliberately one-sided: it refuses only what it can
prove will not fit (`shared_buffers`, WAL buffers, and the lock table implied by
`max_locks_per_transaction × max_connections`). Anything it cannot parse is
allowed through, because a wrong refusal would block a disaster-recovery
restore, while a config PostgreSQL rejects is already caught by the rollback
above.

`parameters put` takes either a bare array or the object that
`oddk parameters get --name <group> --json` prints, so a group round-trips
without `jq`. It refuses a parameter whose `type` is not `postgres_cli_arg`:
that is the only type ODDK applies, so anything else would be stored and then
silently never take effect.

### Notifications

```bash
oddk notify help-add --type email      # print a template for a channel type
oddk notify apply --file notify.json   # apply all channels from a JSON array
oddk notify test                       # send a test to every channel
oddk notify logs --limit 50
```

Supported channels: Email, Slack, Telegram, Webhook. Health degraded/restored
events are delivered automatically with configurable thresholds.

> **Channel credentials are encrypted with the master key** (since 0.1.80) —
> the SMTP password, the Slack webhook URL, the Telegram bot token, and any
> headers you set on a webhook. The whole config blob is encrypted, so a
> credential in a webhook header is covered like any other. Configs written by
> earlier versions are re-encrypted the next time the daemon starts.
>
> Two things it does *not* do. It does not protect a snapshot archive taken
> **before** 0.1.80 — those carry these credentials in the clear, so rotate
> anything that has ridden in one. And it does not protect against someone who
> has both `oddk.db` and `master.key`, which is what an archive plus its key
> amount to. Still prefer a credential you can scope and rotate (a dedicated
> SMTP user, a per-deployment webhook) over one that grants anything else.

An instance left in `error` by a failed operation — an apply whose rollback also
failed, a start that never reached readiness, a switch interrupted by a daemon
restart — is reported as broken rather than skipped, so a deployment cannot read
as healthy while an instance has no working cluster. The message names the
reason, which is what separates a stalled operation from a cluster that simply
stopped answering.
Note that `oddk snapshot apply` deliberately leaves configuration-only instances
in `error`, so a disaster-recovery restore will raise this too; the notification
says so and points at `oddk checklist`.

**Scheduled runs notify on failure.** A scheduled backup or snapshot that ends
with any failed phase sends one message to every configured channel, naming what
failed and why — and distinguishing "no new archive was produced" from "an
archive was produced but does not hold every instance's data" and from "the
archive exists but could not be uploaded or pruned", because those need
different responses. A run that succeeds after a failure sends a short
"Recovered" notice; a clean run after a clean run says nothing. One message per
run, never one per phase.

Without a channel configured, failures are still recorded — `oddk cron logs`
shows every run and its per-phase outcome — but nothing will reach you until you
go looking.

---

## How it works

- **Daemon + CLI in one binary.** The daemon exposes a local HTTP API on
  `127.0.0.1:5442`; the CLI is a thin remote control that talks to it with a
  bearer token.
- **Sequential operations layer.** All state-changing work runs one-at-a-time
  through an executor, preventing races and half-applied changes. Operations are
  uninterruptible by design — a dropped CLI connection never aborts an in-flight
  snapshot, backup, or restore.
- **Docker-native.** Instances are PostgreSQL containers on a dedicated bridge
  network (`10.88.0.0/16`), each bound to the host-local gateway `10.88.0.1`.
- **SQLite state.** Instance config, the snapshot and backup catalogues,
  schedules, health history, and encrypted secrets live in a local SQLite
  database under the data dir.
- **Self-healing startup.** On boot the daemon reconciles stored instance state
  against actual container state and sweeps orphaned temp artifacts from any
  interrupted operation. An instance left mid-operation by a crash is marked
  `error` with the remedy in the log, rather than being left in a transient
  state that health checks would skip.
- **Graceful shutdown.** On `SIGTERM` (i.e. `systemctl stop oddk`) the daemon
  stops accepting new work and waits for the operation in flight to finish —
  operations are uninterruptible by design, so a snapshot, restore or major
  upgrade is allowed to complete rather than being killed part-way. Send the
  signal a second time to stop waiting. The systemd unit allows 30 minutes for
  this; adjust `TimeoutStopSec` if your operations run longer.

## Security

- **Encrypted secrets at rest.** Postgres passwords, S3 keys and notification
  channel configs are encrypted with AES-256-GCM (self-describing `3ncr.org/1`
  format) using a 32-byte master key at `{dataDir}/master.key` (mode `0600`).
  Notification configs joined the list in 0.1.80; configs written earlier are
  re-encrypted the next time the daemon starts, and a snapshot archive taken
  before then still carries them in the clear. The key file is one
  self-describing line so it can be identified wherever it ends up:
  `ODDK-SECRET-MASTER-KEY;V1;<base64url>;<checksum>`. The checksum is the first
  4 bytes of SHA-256 over the base64url text — `printf %s '<payload>' |
  sha256sum | cut -c1-8` — and exists to tell "the right key, copied badly"
  apart from "the wrong key". Key files written by ODDK <= 0.1.59 were a bare
  base64url string; they are still read and are rewritten in place on the next
  daemon start (the key material never changes). Because ODDK <= 0.1.59 cannot
  read the new format, that first start also saves the previous file as
  `master.key.pre-v1` — if you roll the binary back, restore it with
  `mv master.key.pre-v1 master.key`.
- **Secrets stay out of container metadata.** Since 0.1.81 an instance's
  postgres password is never written into its container's Docker config, which
  Docker keeps for the container's lifetime and hands to anything that can read
  container metadata. A fresh cluster is initialised with a throwaway and the
  real password is set over SQL once it is ready, so `docker inspect` shows a
  value that authenticates nothing; recreating a container (`instance
  apply`/`switch`/`update`) passes no password at all. Containers created before
  0.1.81 keep their old value until their next recreate — clearing it sooner
  would mean restarting the database purely to tidy metadata. Note this is
  hygiene rather than a boundary: anyone who can read that metadata can also
  `docker exec ... psql -U postgres`, which the cluster answers over its local
  socket without a password.
- **Tokenized API auth.** Tokens are Argon2-hashed and compared in constant
  time; the plaintext is shown only at creation.
- **Loopback by default.** The API binds `127.0.0.1`. `--allow-remote` exists
  but sends the token over cleartext HTTP — prefer `ssh -L 5442:localhost:5442`.
- **Host-local Postgres.** Containers bind the Docker bridge gateway, not a
  public interface.
- **Unprivileged service user.** The daemon runs as the `oddk` user with no
  login shell.

The threat model is a **single trusted operator on a single host**. ODDK is not
hardened for hostile multi-tenant use.

---

## Building from source

```bash
make build        # build the single binary into ./bin/oddk
make test         # unit tests
make test-e2e     # end-to-end tests (requires Docker)
make test-all     # both
make lint         # golangci-lint (managed via `go tool`, no separate install)
```

Run the daemon directly during development:

```bash
./bin/oddk daemon [--port 5442] [--data-dir ./data] [--backup-dir ./backups]
```

The daemon does not mint a token itself. Provision a CLI config with
`oddk auth mint` (run as the data-dir owner — in dev that's just you, so no
`sudo` needed; `--json` prints the config instead of eval-able shell). The CLI
reads `.oddk-cli.json` in the current directory or `~/.config/oddk/cli.json`.

**Toolchain:** Go 1.26+, Docker. Linux (primary) or macOS (development).

## License

MIT — see [LICENSE](LICENSE).
