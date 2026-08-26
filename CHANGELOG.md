# Changelog

Notable changes to ODDK, newest first.

Entries describe what changed **for a running deployment** — what you get, what
behaves differently, and what (if anything) you have to do about it. Anything
labelled **Action** needs a decision or a command from you; everything else
takes effect on its own when you upgrade. The reasoning behind a change lives in
the commit that made it.

Versions are `0.1.x`: during development every release bumps the patch number,
whether it carries a feature, a fix or a refactor.

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
