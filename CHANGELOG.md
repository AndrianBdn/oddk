# Changelog

Notable changes to ODDK, newest first.

Entries describe what changed **for a running deployment** — what you get, what
behaves differently, and what (if anything) you have to do about it. Anything
labelled **Action** needs a decision or a command from you; everything else
takes effect on its own when you upgrade. The reasoning behind a change lives in
the commit that made it.

Versions are `0.1.x`: during development every release bumps the patch number,
whether it carries a feature, a fix or a refactor.

## v0.1.85 — 2026-09-16

### Fixed

- **Backups and snapshots larger than 5 GiB can upload to S3.** Large archives
  upload in parts, with retries per part and cleanup on failure or cancellation.
  Uploads stream from disk with two concurrent parts.
- **Local retention protects large snapshots that have no offsite copy.** The
  previous exception for archives above 5 GiB is removed. Migration previews no
  longer warn about the old upload cap.

**Action:** Upgrade ODDK and restart its daemon. Allow `s3:AbortMultipartUpload`
on the archive prefix in the bucket's upload policy. Configure an S3 lifecycle
rule to abort incomplete multipart uploads left by crashes or failed cleanup.
For SSE-KMS buckets, the upload identity also needs `kms:Decrypt` alongside
`kms:GenerateDataKey` on the encryption key. No database migration is required.

## v0.1.84 — 2026-09-05

### Fixed

- **Deleting a database user no longer risks deleting objects created during
  ownership reassignment.** Each database's reassignment and privilege cleanup
  now run in a transaction that refuses object deletion. If concurrent DDL would
  cause data loss, the command preserves the object and the role and asks you to
  stop the application's DDL before retrying. Cleanup already completed in other
  databases is not rolled back.
- **Container rollback uses the previous image's immutable ID.** If an update
  or image switch moves a tag to a broken image, rollback restores the image the
  old container actually used instead of retrying the broken tag. The previous
  image is checked before the existing container is removed.
- **Adding a database user is atomic.** Role creation, grants, and optional
  ownership transfer now share one transaction. A failed grant or ownership
  change no longer leaves a partially configured user or changed database owner
  behind without returning the generated password.
- **`add-db-user --owner` handles stored procedures and aggregates**, as well as
  functions. Ownership statements explicitly name the public schema, so a
  customized `search_path` cannot redirect them.

### Changed

- Parameter-file decoding is shared by the CLI and API; both still accept a
  bare array or the object emitted by `parameters get --json`. S3-location
  parsing is shared by backup and snapshot operations and rejects locations
  with an empty bucket or object key.
- Removed the E2E runner's unused `-v` and `-parallel` flags.

**Action:** Upgrade ODDK and restart its daemon before managing database users
or changing instance images. No database migration is required.

## v0.1.83 — 2026-09-04

### Added

- **`oddk instance apply` now resizes and re-ports an instance, not only its
  parameter group.** `--cpu`, `--ram` and `--port` can be given in any
  combination with `--parameter-group`; flags you do not pass keep their
  current value. The change goes through the same path as a group change: the
  container is recreated (a brief restart, the data volume untouched), what can
  be refused is refused before the running container is touched — more CPU or
  RAM than the host has, a port another instance holds or something already
  answers on, a group whose shared memory will not fit the *new* RAM — and a
  container that comes up but never reaches readiness is rolled back to the
  previous shape. Because parameter groups resolve against the instance's RAM,
  a resize re-derives `shared_buffers` and friends automatically. Until now CPU,
  RAM and port were fixed at `create` for the life of the instance, which was
  the biggest gap against the "local RDS" positioning.
  `PUT /api/rdbms/{name}/config` accepts the new fields (`cpuCores`, `ramMb`,
  `port`, all optional); a body with only `parameterGroup` behaves as before.

### Security

- **Built with Go 1.27.1** (v0.1.82 was built with 1.26.5). A `govulncheck` of
  the previous binary reported six standard-library vulnerabilities as
  reachable: quadratic path resolution in `net/url`, unbounded post-handshake
  messages in `crypto/tls`, two in `net/http` (the unencrypted HTTP/2 check
  ignoring `ReadHeaderTimeout`, and Punycode label handling), and missing
  recursion guards in `encoding/xml` and `encoding/asn1`. Exposure was limited
  by design — the daemon listens on loopback and only talks to Docker over its
  socket, to S3, and to the notification endpoints you configured — but fixing
  all six costs one line. `golang.org/x/text` moves to 0.39.0 for the same
  reason (an infinite loop on invalid input).

### Changed

- **The Docker client library is now `github.com/moby/moby/client`**, the
  client Docker Engine 29 ships, replacing `github.com/docker/docker`. That
  module stopped receiving releases at 28.5 and carries two Moby
  plugin-authorization advisories with no fix on its import path. ODDK never
  ran the affected code — it only talks to the daemon over its socket — but a
  vulnerability report that can never go green is one nobody reads. Nothing
  changes for a deployment: the same API calls are made, and the client still
  negotiates the API version with whatever Docker Engine is installed.
- Every direct dependency is on its current release. The notable jumps: the
  SQLite driver (`modernc.org/sqlite` 1.38 → 1.58), the CLI framework
  (`urfave/cli` 3.4 → 3.11), the AWS SDK's S3 client (1.97 → 1.110), `pgx`
  5.9 → 5.10 and `klauspost/compress` 1.18 → 1.20. No behaviour change is
  intended. If `oddk` misbehaves after this upgrade in a way these notes do not
  explain, the SQLite driver bump is the first suspect.

### Fixed

- **An authentication failure is recognised by its SQLSTATE, not by the English
  text of the server's message.** PostgreSQL localizes server messages through
  `lc_messages`, which any parameter group can set, so on a cluster running
  with a non-English locale the probe behind `oddk instance status` and the
  checklist reported `PostgreSQL connectivity issue (other)` where it should
  have said `PostgreSQL authentication failed`, and `add-db-user` on a missing
  database returned a generic error instead of the 404 with the `create-db`
  hint. S3 "no such object" and Docker "no such container" answers are now
  recognised by their typed errors as well. Nothing changes where the messages
  were English.
- **`oddk snapshot apply` refuses a truncated `--logical` archive before it
  replaces anything.** A logical archive missing its `databases.json` or
  `globals.sql` was discovered only while rebuilding the instance, which is
  after `oddk.db` and `master.key` had been installed — an apply that could
  not simply be retried. The physical format already had this check in
  preflight; the logical one now does too, and both `apply` and
  `restore-instance` build a cluster through one shared function, so the two
  cannot drift apart on this again.
- `oddk backup remove-remote` and `oddk snapshot remove-remote` no longer fail
  with a bare `EOF` when the S3 delete stalls past the daemon's 30-second
  response deadline; the removal completed on the daemon either way. The rule
  behind this and the `delete-db-user` fix below is now enforced by a test, so
  a new long-running handler cannot ship without it.
- `oddk instance delete-db-user` no longer fails with a bare `EOF` on a large
  database. `REASSIGN OWNED BY` locks every object the user owns in one
  transaction and could outlive the daemon's 30-second response deadline; the
  operation then completed on the daemon while the CLI reported a connection
  error, leaving you to check whether the user was actually gone. The handler
  now clears the deadline like every other long-running operation.
- `install.sh` no longer tells the owner of a legacy `/home/oddk` install to
  run a relocation script that was retired in v0.1.45. It says what has to
  move instead.

## v0.1.82 — 2026-08-26

### Fixed

- **An instance left in `error` no longer displays as `stopped`.** `oddk list`,
  `oddk instance status` and the per-instance header in `oddk checklist` derive
  what they show from the container, which is what makes a database somebody
  stopped out of band read `stopped` rather than `running`. But it also
  overwrote `error` — the one status that is never cleared on its own — so an
  operation that failed *and* whose rollback failed left the row saying `error`
  while every list command said `stopped`, the word for an instance stopped on
  purpose. The same `oddk checklist` run printed `instance_error:<name>` and a
  failing health line two lines away from the header contradicting them, and
  nothing cleared the row, so it read that way indefinitely. Health checks and
  notifications were always driven by the stored value and were unaffected.

### Changed

- `oddk instance major-upgrade` now pulls the target image itself if it is
  missing, like `create`, `switch` and `update` already did, instead of
  stopping at the confirmation prompt to send you away for a separate
  `oddk pull`. The pull happens after you confirm, so a cancelled upgrade
  fetches nothing, and an upgrade that cannot happen on version grounds — a
  downgrade, or the major the instance already runs — is still refused
  instantly, without fetching anything.

  One message changed with it: a `--target-version` whose image exists in no
  registry now fails naming the image that could not be resolved, rather than
  reporting it missing locally and advising a pull that would have failed the
  same way.

## v0.1.81 — 2026-08-25

### Changed

- **An instance's postgres password is no longer written into its container's
  Docker configuration.** Docker keeps `Config.Env` for a container's whole
  lifetime and hands it to anything that can read container metadata, so the
  credential was reachable through `docker inspect`, a backup of
  `/var/lib/docker`, or an observability agent holding a read-only
  `docker.sock` mount. A new cluster is now initialised with a throwaway
  password and the real one is set over SQL once it is ready, so what remains
  visible authenticates nothing. Recreating a container (`instance apply`,
  `switch`, `update`) passes no password at all.

  This is hygiene, not a closed exposure: anyone who can read that metadata can
  also `docker exec <container> psql -U postgres`, which the cluster answers
  over its local socket without a password. No action is required.

  Containers created before this release keep their old value until their next
  recreate — clearing it sooner would mean restarting the database purely to
  tidy metadata. `oddk instance update <name>` will do it if you want it gone
  now, at the cost of a brief restart.

- A recreate that meets an unexpectedly empty data volume now refuses to start
  instead of initialising a fresh empty cluster in place of your data.

## Earlier releases

This file starts at v0.1.81. The entries below are a **curated** list of earlier
changes that a deployment upgrading from an older version should know about —
not a complete history. For that, read the commit log: every release is one
commit whose subject describes the change.

- **v0.1.80** (2026-08-24) — Notification credentials (`notifications.config`)
  are encrypted at rest with the master key. They previously rode into every
  snapshot archive in cleartext, and archives are unencrypted and uploaded to
  S3.
  **Action:** encrypting the column going forward does not unpublish what
  already shipped. Rotate any SMTP password, Slack webhook URL, Telegram bot
  token or webhook `Authorization` header that has ridden in an archive.

- **v0.1.79** (2026-08-23) — Retention's pin on the newest *complete* archive is
  now bounded (its tier's window plus 30 days), so a permanently degraded
  instance can no longer pin one archive forever. `snapshot apply` gained
  `--no-pause-schedules` for a genuine failover where the source host is gone.
  `backup list-cron` gained a STATUS column, so a paused schedule no longer
  renders as a live one.

- **v0.1.78** (2026-08-23) — `snapshot apply` now **pauses** every restored
  schedule. A restored `oddk.db` carries the source host's offsite credentials
  and bucket path, so an unpaused DR host would upload into that bucket and run
  offsite retention against it, deleting objects the source still catalogues.
  **Action:** after a real failover, resume deliberately —
  `oddk snapshot setup-cron --resume`, and
  `oddk backup setup-cron --instance <name> --resume` for any legacy per-instance
  schedules. `oddk checklist` and a daily notification will keep telling you
  until you do.

- **v0.1.77** (2026-08-23) — A parameter-group apply that cannot start no longer
  costs you the running instance: preflight failures leave the previous
  container serving, and anything later rolls back to it.

- **v0.1.75** (2026-08-12) — A snapshot that silently captured no data for an
  instance is now reported as a failure (non-zero exit, failed capture phase,
  one notification) instead of a success, and the archive is still kept and
  shipped. Downloaded archives are now `0600` like written ones.

- **v0.1.73** (2026-08-11) — Archives are verified on the way **in** as well as
  out, so a corrupt download can no longer be catalogued as a good local copy or
  cached for reuse by later restores.

- **v0.1.71** (2026-08-11) — One instance's capture failure no longer aborts the
  whole snapshot. That entry degrades to configuration-only and the run
  continues, so a single sick instance cannot cost every other instance its DR
  archive.

- **v0.1.70** (2026-08-11) — Every archive is written atomically and durably and
  read back before it is published, and the restore path can now actually detect
  corruption. Previously a truncated archive could be catalogued as complete.

- **v0.1.69** (2026-08-11) — Read-only commands (`list`, `checklist`, `cron
  logs`, …) answer while a snapshot runs, instead of blocking and then failing
  with a bare `EOF`.

- **v0.1.67** (2026-08-11) — A scheduled run that fails now sends one
  notification naming each failed phase and its cause, and `oddk cron logs`
  exists to read that history. Before this, nothing reported a failed backup,
  snapshot or upload.

- **v0.1.66** (2026-08-11) — Snapshot capture dispatches on the container's
  actual Docker state rather than the stored status, which could drift and
  silently reduce a healthy instance to configuration-only in every archive.

- **v0.1.65** (2026-08-07) — S3-native restore: `snapshot list-remote`,
  `snapshot restore-instance --id/--s3-uri`, and `snapshot apply --s3-uri`.

- **v0.1.63** (2026-08-06) — `oddk backup dangerously-drop-all` for
  decommissioning the legacy per-instance backup system in one sweep.

- **v0.1.62** (2026-08-05) — `oddk checklist` became snapshot-centric: backups
  are legacy and no longer audited, and per-instance snapshot coverage is
  detected from the archive rather than assumed.

- **v0.1.61** (2026-08-04) — Snapshots are **physical** (`pg_basebackup`) by
  default: faster, no locks, and byte-for-byte restores that preserve
  per-database GUCs, database-level ACLs and ICU collations. `--logical` selects
  the previous portable format, which you need for cross-architecture moves and
  for unlogged tables.
  **Action:** none to upgrade, but existing snapshot schedules were backfilled
  to `physical`. Use `oddk snapshot setup-cron --logical` if you need the
  portable format.

- **v0.1.60** (2026-07-31) — `master.key` became a self-describing single line
  (`ODDK-SECRET-MASTER-KEY;V1;…`), upgraded in place on first start. The old
  bare-base64 format is still read. Also added
  `oddk snapshot migrate-from-backups` for moving a deployment off per-instance
  backup schedules.
