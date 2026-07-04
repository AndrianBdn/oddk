# ODDK — Opinionated Database Deployment Kit

<a href="https://github.com/Hypersequent/oddk/releases/latest"><img src="https://img.shields.io/github/v/release/Hypersequent/oddk" /></a>
<a href="./LICENSE"><img src="https://img.shields.io/github/license/Hypersequent/oddk" /></a>
<a href="./go.mod"><img src="https://img.shields.io/github/go-mod/go-version/Hypersequent/oddk" /></a>
[![Go Report Card](https://goreportcard.com/badge/github.com/hypersequent/oddk)](https://goreportcard.com/report/github.com/hypersequent/oddk)

**Run PostgreSQL on your own Linux box with the ergonomics of a managed service.**

ODDK is a single Go binary that manages PostgreSQL the way a cloud provider's
managed database does — create an instance, get a connection string, take
scheduled backups, ship them offsite to S3, watch health, restore on demand,
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
- **Operationally complete.** Backups (local + S3 offsite), scheduled daily
  backups with retention, point-in-time-style restore from any archive, health
  monitoring with Email/Slack/Telegram/Webhook alerts, password and user
  management, minor-version image switches, and dump/restore major upgrades.
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
| Confidence it's backed up | `backup make`, scheduled cron backups, S3 offsite with retention |
| To not lose data | `backup restore` from any local or downloaded archive |
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
host disk/CPU/memory for health checks, and writes state and backups to host
paths. That is the opposite of being a containerized workload itself. Running
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
curl -fsSL https://raw.githubusercontent.com/Hypersequent/oddk/main/install.sh | sh
```

Pin a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/Hypersequent/oddk/main/install.sh | sh -s -- --version v0.1.39
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
(`oddk instance --help`, `oddk backup --help`, …).

### Instances

```bash
oddk create --name app --version 17 --port 5432 --cpu 4 --ram 8
oddk create --name dev --version 17 --port 5433 --cpu 1 --ram 1024M   # RAM accepts M/MB/MiB
oddk instance status app
oddk instance start app
oddk instance stop app
oddk instance logs app --follow
oddk instance destroy app
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

### Passwords

```bash
oddk instance get-postgres-password app                 # structured details
oddk instance get-postgres-password app --plain         # just the password
oddk instance get-postgres-password app --conn          # connection string
NEW_PGPASSWORD=secret oddk instance set-postgres-password app
```

### Backups

```bash
oddk backup make app --comment "before deploy"
oddk backup list --instance app
oddk backup restore --instance app --id 42 --database analytics
oddk backup restore --instance app --id 42 --database analytics --restore-as analytics_copy
oddk backup restore --instance app --file /path/to/backup.tar.zst --database analytics
```

### Scheduled & offsite backups

```bash
# Configure S3 offsite (see `oddk offsite get` for the config template)
oddk offsite apply --file offsite.json
oddk offsite test

# Schedule a daily backup at 03:00 UTC; uploads offsite when configured
oddk backup setup-cron --instance app --utc-hour 3
oddk backup list-cron

# Move copies around
oddk backup upload app <backup-id>
oddk backup download app <backup-id>
```

When offsite is configured, failed uploads are retried on later cron runs, and
local retention never deletes a backup whose only copy is local.

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

> `create`, `switch`, and `update` pull the image automatically when needed —
> `oddk pull` is optional, for pre-warming or CI. Quiesce writes before a major
> upgrade; changes made after it starts are not migrated. Cross-major `switch` is
> rejected up front — use `major-upgrade`.

### Parameter groups (AWS-style tuning)

```bash
oddk parameters get                                   # list groups
oddk parameters get --name default:2025-08-27         # inspect one
oddk parameters put custom --file params.json         # create/update
oddk create --name app --version 17 --port 5432 --cpu 4 --ram 8 --parameter-group custom
oddk instance apply app --parameter-group custom      # reconfigure in place
```

Parameters support expression evaluation against the instance's resources, e.g.
`"{expr}DBContainerMemoryMB / 4{/expr} MB"` for `shared_buffers`.

### Notifications

```bash
oddk notify help-add --type email      # print a template for a channel type
oddk notify apply --file notify.json   # apply all channels from a JSON array
oddk notify test                       # send a test to every channel
oddk notify logs --limit 50
```

Supported channels: Email, Slack, Telegram, Webhook. Health degraded/restored
events are delivered automatically with configurable thresholds.

---

## How it works

- **Daemon + CLI in one binary.** The daemon exposes a local HTTP API on
  `127.0.0.1:5442`; the CLI is a thin remote control that talks to it with a
  bearer token.
- **Sequential operations layer.** All state-changing work runs one-at-a-time
  through an executor, preventing races and half-applied changes. Operations are
  uninterruptible by design — a dropped CLI connection never aborts an in-flight
  backup or restore.
- **Docker-native.** Instances are PostgreSQL containers on a dedicated bridge
  network (`10.88.0.0/16`), each bound to the host-local gateway `10.88.0.1`.
- **SQLite state.** Instance config, backups, schedules, health history, and
  encrypted secrets live in a local SQLite database under the data dir.
- **Self-healing startup.** On boot the daemon reconciles stored instance state
  against actual container state and sweeps orphaned temp artifacts from any
  interrupted operation.

## Security

- **Encrypted secrets at rest.** Postgres passwords and S3 keys are encrypted
  with AES-256-GCM (self-describing `3ncr.org/1` format) using a 32-byte master
  key at `{dataDir}/master.key` (mode `0600`).
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
