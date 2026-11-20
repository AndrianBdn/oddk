# Changelog

Notable changes to ODDK, newest first.

Entries describe what changed **for a running deployment** — what you get, what
behaves differently, and what (if anything) you have to do about it. Anything
labelled **Action** needs a decision or a command from you; everything else
takes effect on its own when you upgrade. The reasoning behind a change lives in
the commit that made it.

Versions are `0.1.x`: during development every release bumps the patch number,
whether it carries a feature, a fix or a refactor.

## v0.1.97 — 2026-11-20

PostgreSQL 19 compatibility.

### Added

- **PostgreSQL 19 support.** `oddk create --version 19`, and
  `oddk instance major-upgrade <name> --target-version 19` from an earlier
  major. Physical snapshots, `restore-instance` and `restore-database` work on
  19 as they do on 18 (the image keeps 18's data-directory layout).

### Changed

- **`oddk create` defaults to PostgreSQL 18** (was 17) when given neither
  `--version` nor `--image`. Existing instances are not affected.
- **With `--image` and no `--version`, the major now comes from the image**: the
  `PG_MAJOR` it declares (every image built on the official one does), else a
  major in its tag. An image that names its major neither way is refused with a
  request for `--version`; it used to get the default, and a default that did
  not match the image put the data where the next container recreate would not
  find it. A `--version` that contradicts the image's `PG_MAJOR` is refused for
  the same reason, and so is an image whose tag names a different major than
  its `PG_MAJOR` (retag it with its major).
  **Action:** a script that creates without `--version` from an image that names
  its major neither way must now pass `--version`; an image whose tag names the
  wrong major must be retagged.

### Fixed

- **`major-upgrade` checks the parameter group against the target major before
  it changes anything.** The new cluster is started with the instance's group
  only after the old volume has been removed, so a setting the new major does
  not accept left the instance in `error`, with the pre-upgrade backup as the
  only way back. The target image's server now validates each setting's name
  and value first, and the group's fit in the instance's RAM is checked; a
  refusal names the setting and changes nothing, not even a backup is taken.
  Not covered: a `shared_preload_libraries` library missing from a custom
  target image, and rules that combine several settings; those still surface
  at the new cluster's first start.
- `oddk create` without `--version` or `--image` failed at the automatic image
  pull ("version or image is required"). It now uses the default.

### Notes for PostgreSQL 19

- **Action:** if an 18 instance's parameter group sets `io_workers`, remove it
  before upgrading that instance to 19 (19 replaced it with
  `io_min_workers`/`io_max_workers`). The upgrade refuses until you do. The
  group ODDK ships does not set it.
- 19 counts lock-table capacity differently: a group that sets
  `max_locks_per_transaction` gets about half the capacity it had on 18. Double
  it if you set it for a workload that needs many locks.
- JIT is off by default in 19; turn it back on in the group (`jit=on`) if
  large analytical queries relied on it.
- Logging in with an MD5 password now produces a warning. Roles ODDK creates use
  SCRAM unless a parameter group sets `password_encryption=md5`.

## v0.1.96 — 2026-11-05

### Fixed

- **`oddk snapshot apply` no longer refuses an instance recorded with more CPU
  or RAM than the target host has.** An instance keeps its size when its host
  is resized down, so a host could not restore its own snapshot onto an
  identical machine ("requested RAM (8192 MB) exceeds available memory"). Apply
  now restores such an instance at what the host provides, builds its container
  (memory limit, `shm_size`, memory-derived parameters) from that size, and
  records it in `oddk.db`. Preflight reports it:
  `✓ Host can run the recorded instance sizes (qasphere: 8192 → 7762 MB to fit this host)`.
  `snapshot restore-instance` does the same and reports it on a `Resized:`
  line (and as `resized` in `--json`). Snapshots written by earlier
  versions apply as they are.
- A parameter group that sets memory explicitly (for example `shared_buffers`)
  and does not fit the instance's RAM is now refused in preflight, before
  `oddk.db` and `master.key` are replaced, when the archive carries the group's
  definition (every snapshot does unless the group was unreadable at capture).
  It used to fail once the destructive phase had begun.

### Added

- **`oddk checklist` flags an instance sized above its host**, which is how the
  apply refusal above used to arise. Lower it with
  `oddk instance apply <name> --cpu <cores> --ram <MB>M`; both values are
  checked against the host.

## v0.1.95 — 2026-11-05

### Added

- **`oddk snapshot verify <id>` (or `--all`)** checks stored copies before a
  restore depends on them. The local copy is read in full (decrypted and
  decompressed) and its SHA-256 compared with the one recorded when it was
  written; a snapshot from before v0.1.86, which has no recorded SHA-256, gets
  its structure checked instead. The S3 copy is checked for existence and size, and
  `--deep` downloads and verifies it the same way without writing it to disk.
  Any copy that is missing, damaged, replaced or could not be checked makes the
  command exit non-zero, so it can run from cron. It does not wait for a running
  snapshot.

### Fixed

- **A `--readonly` user can now read sequences created after it was added**
  (by `postgres` or the database owner, as for tables), so `pg_dump` as that
  user no longer fails with "permission denied for sequence" once a table with a
  `serial` or identity column appears. Sequences that existed when the user was
  added were already covered.
- **Action:** read-only users created before this release keep the old grants.
  Recreate them (`delete-db-user`, then `add-db-user --readonly`), or run this in
  each database, as `postgres`, once for every role that creates tables there
  (`postgres` and the database owner):
  `ALTER DEFAULT PRIVILEGES FOR ROLE <creator> IN SCHEMA public GRANT SELECT ON SEQUENCES TO <reader>;`
  plus `GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO <reader>;` for the
  sequences created in the meantime.

## v0.1.94 — 2026-10-17

### Added

- **Archives are encrypted at rest.** Every snapshot, legacy backup and
  pre-upgrade backup is now written as `.tar.zst.age`: [age](https://age-encryption.org)
  with a post-quantum hybrid key (ML-KEM-768 + X25519) derived from `master.key`.
  Someone who can read the backup directory or the offsite bucket but does not
  have `master.key` learns nothing about the contents, not even instance names.
  There is no new secret: restore and apply use the `--master-key` they already
  take. There is no setting to turn it off.
- **A wrong master key is refused before anything changes,** and the error names
  the key the archive needs: the checksum that ends its `master.key` line.
  `snapshot apply` now checks the key first, before any other preflight step.
- **`oddk snapshot decrypt`** writes the plain `.tar.zst` out of an encrypted
  archive (verified first, mode 0600). With `--print-age-identity` it prints the
  derived age secret instead, so the stock `age` CLI can decrypt with no ODDK
  installed. It runs locally; no daemon needed.

### Changed

- Archives written before this release keep their `.tar.zst` name and stay
  restorable through every path, unchanged.
- An `--s3-uri` download that this host cannot decrypt is now discarded, because
  it cannot be verified, rather than kept for a retry. Supply the source's
  `--master-key` and the retry downloads it again.
- A legacy `backup restore` of a backup written on **another** host is refused:
  backups have no `--master-key`. Snapshots are the cross-host path.

**Action:** without `master.key`, archives from this release on cannot be
restored or read. Confirm now that `master.key` is backed up somewhere other
than this host and its bucket.

**Action:** ODDK versions before 0.1.94 cannot read the new archives. Install
this release or later on any host you would restore or apply to.

**Action (optional):** archives already in your bucket stay **unencrypted**
until retention prunes them. To be rid of them sooner, delete them once a newer
encrypted *complete* snapshot exists (`oddk snapshot list`,
`oddk snapshot remove-remote <id>`).

## v0.1.93 — 2026-09-30

### Fixed

- **`snapshot restore-instance --json` and `snapshot restore-database --json`
  print only JSON on stdout.** Progress lines and credential notes ("Restoring
  …", "No AWS credentials in this shell") used to come first, so a script
  parsing the output failed *after* the restore had already run. A retry was
  then refused, because the database existed, or rebuilt the instance a second
  time. Those lines now go to stderr in JSON mode.
- **`--json` on these two commands now requires `--yes`,** as it already did for
  `migrate-from-backups` and `dangerously-drop-all`. The confirmation prompt
  would otherwise land in the JSON output.

**Action:** add `--yes` to any script that runs either command with `--json`.

## v0.1.92 — 2026-09-30

### Deprecated

- **Per-instance backups (`oddk backup`) will be removed in the first release
  after 2026-12-31.** Snapshots now cover everything backups did, including
  restoring or cloning a single database (`oddk snapshot restore-database`,
  v0.1.87).
  - Every `oddk backup` command prints a notice on stderr naming its
    replacement. Stdout and `--json` output are unchanged, so scripts keep
    working.
  - **New backup schedules are refused.** Existing schedules keep running and
    can still be changed, paused, resumed or removed.
  - Restore, list, download and `dangerously-drop-all` keep working until the
    removal, so existing archives stay usable.

**Action:** if `oddk checklist` shows a `legacy backups` warning, move the
schedules before the removal with `oddk snapshot migrate-from-backups --dry-run`
(preview), then `--yes`. When you trust the snapshot schedule, clear the old
archives with `oddk backup dangerously-drop-all`. Anything that creates backup
schedules through `oddk backup setup-cron` or `POST /api/cron/backup` must
switch to `oddk snapshot setup-cron`.

## v0.1.91 — 2026-09-29

### Fixed

- **A whole-instance restore makes no connection that fires the restored
  cluster's login triggers** (PostgreSQL 17+). After a physical `restore-instance`
  or `snapshot apply`, the readiness check and database count connected
  normally. After a logical one, the final verification did. Each of those
  connections fired any login trigger in the restored `postgres` database. An
  audit trigger recorded them as user logins, and a trigger that raises an error
  made a completed restore report failure. Once a restore finishes, the
  cluster's triggers fire on logins as they always did, including ODDK's health
  checks.

**Action:** None.

## v0.1.90 — 2026-09-29

### Fixed

- **Restores no longer fire the database's own login event triggers**
  (PostgreSQL 17+). `pg_restore` recreates a login trigger near the end of a
  restore and then reconnects. That reconnect ran the trigger against the
  database it had just restored. A trigger that deletes rows emptied the
  restored database, and an audit trigger logged the restore as a user login.
  Every restore now runs with event triggers off: `backup restore`, `snapshot
  restore-database`, a logical `restore-instance` or `apply`, and `instance
  major-upgrade`.
- **The scratch copy used by `snapshot restore-database` fires no login trigger
  either,** and its read-only setting can no longer be overridden by a role or
  database setting inside the archive. Its clients set both at connect time,
  where they take precedence. Before this, a login trigger could change the
  copy before it was dumped. Even a harmless audit trigger made the restore
  fail.
- **A restore extracts only bytes it verified.** Since v0.1.89 a rewrite of the
  archive during a restore was detected by its size and modification time. A
  different archive of the same length with its timestamp preserved got past
  that check. The bytes read for extraction are now hashed and must match the
  bytes that were verified. This costs one SHA-256 pass over the archive per
  restore.

**Action:** None.

## v0.1.89 — 2026-09-29

### Fixed

- **A restore extracts exactly the archive it verified.** The archive was
  verified, closed, and then opened again by name to extract. A file replaced
  in between, for example by a sync job or a `cp` into the backup directory,
  passed the checks as one archive while another was restored. Verification
  and extraction now read the same open file. An archive rewritten in place
  while a restore reads it is refused, and the extracted files are discarded,
  before anything is changed. This covers every restore: `backup restore`,
  `snapshot restore-instance`, `snapshot restore-database`, `snapshot apply`
  and `instance major-upgrade`.
- **The scratch copy used by `snapshot restore-database` cannot be left behind
  unnoticed.** Docker can report a container removed while failing to delete
  its volume, which here holds a full copy of an instance's data. The volume is
  now created with an ODDK label and removed by a separate, checked call. If
  that fails, the command says so and prints the removal command. The daemon
  also removes any leftover scratch volume when it starts.

**Action:** None.

## v0.1.88 — 2026-09-29

### Fixed

- **Database names are always passed to PostgreSQL tools as a name.** A name
  containing `=`, or starting with `postgres://` or `-`, was read by
  `pg_restore`, `pg_dump` and libpq as a connection string or an option.
  `backup restore --restore-as 'dbname=postgres'` could pass the "target does
  not exist" check and then write into the existing `postgres` database. This
  affected `backup restore`, `instance major-upgrade`, and backups and logical
  snapshots of such a database.
- **A database whose name contains a space or other punctuation can be backed
  up.** The name was used in a helper container's name, which Docker rejects.
- **Restoring by `--id` checks the local copy against the recorded SHA-256.**
  Since v0.1.86 this was checked only when the archive had to be downloaded.
  A catalogued local file that had been replaced by another valid archive was
  restored as if it were the original. This covers `backup restore --id`,
  `snapshot restore-instance --id` and `snapshot restore-database --id`. The
  check runs in the verification pass that already happens before extraction,
  so it adds no extra read. Archives recorded before v0.1.86 have no digest and
  are not checked.

**Action:** None.

## v0.1.87 — 2026-09-29

### Added

- **`oddk snapshot restore-database` restores one database out of a snapshot.**
  It creates the database on a running instance and changes nothing else. The
  instance's other databases, roles, postgres password and configuration stay as
  they were, and it keeps serving. The target name must be free, so use
  `--restore-as` to put yesterday's copy next to today's. It takes the same
  `--file`, `--id` and `--s3-uri` sources as `restore-instance`. As with `backup
  restore`, objects come back owned by `postgres`, without object privileges.
  Until now, restoring a single database still needed a per-instance backup.
- **`--from-instance` restores another instance's database.** For example,
  `--instance staging --from-instance prod --database sales` puts last night's
  production database on staging. The target does not have to be in the
  snapshot. It must be running a PostgreSQL major at least as new as the
  snapshot's copy.
- **From a physical snapshot (the default format), the instance's copy is
  started as a scratch cluster first,** because a physical archive has no
  per-database dumps. The database is dumped out of that cluster, and then the
  cluster and its volume are removed. The scratch cluster has **no network
  access**, and it runs no background workers, no logical replication and no
  WAL archiving. So the source's subscriptions, foreign servers and scheduled
  jobs (pg_cron, TimescaleDB and pg_partman workers) can neither reach real
  systems nor change the copy before it is dumped. It needs free disk for the
  whole cluster while it runs, and UNLOGGED tables come back empty.

### Fixed

- **Helper containers no longer leave a Docker volume behind.** Every `pg_dump`,
  `pg_restore` and `pg_basebackup` helper got an anonymous volume from the
  postgres image, and removing the helper did not remove it. A host that has run
  nightly backups for a while has thousands of them. They are small, but each is
  a directory and a Docker record.

**Action:** optional cleanup. ODDK cannot tell its leaked volumes from anyone
else's because they carry no label, so it does not remove existing ones. List
them with `docker volume ls -qf dangling=true`. If nothing else on the host
relies on unattached anonymous volumes, reclaim them with `docker volume prune`.
On Docker 23 and later, that command removes only anonymous volumes.

## v0.1.86 — 2026-09-29

### Added

- **Every new backup and snapshot records its SHA-256 in the catalogue.** The
  digest is of the archive file, the same value `sha256sum` prints. It is taken
  from the read-back that already verifies every archive before it is
  published, so it describes bytes that were proven intact. It appears as
  `sha256` in `snapshot list --json`, `snapshot make --json`, and the backup
  records returned by `GET /api/backups`.
- **Downloading a catalogued archive checks it against that digest.** This
  covers `backup download`, `snapshot download <id>`, and `snapshot
  restore-instance --id` when it has to fetch the archive. Corruption was
  already refused. What is new is refusing an archive that is **intact but is
  not the one this host wrote**: an object replaced in the bucket, for example.
  Previously that downloaded cleanly and would have restored someone else's
  data. The download is now discarded, and the error gives both digests.
  `--s3-uri` fetches are not tied to a catalogue row, so they are not checked.

Archives recorded before this version have no digest. They are not checked,
and each one records the digest of its first verified download instead. A
recorded digest is never replaced.

Writing and downloading archives costs one extra SHA-256 pass over the file,
done in the same read. Measured at about 1.8 s/GB on a CPU without SHA
extensions; CPUs that have them are several times faster. Restores are
unaffected.

**Action:** None. Migration 022 adds the column on the first daemon start.

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
  bare-base64 format is still read. ODDK <= 0.1.59 cannot read the new format,
  so that first start saves the previous file as `master.key.pre-v1`: to roll
  the binary back, restore it with `mv master.key.pre-v1 master.key`. Also added
  `oddk snapshot migrate-from-backups` for moving a deployment off per-instance
  backup schedules.
