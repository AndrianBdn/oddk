package operations

// Per-instance backups are deprecated in favour of whole-deployment snapshots
// and will be REMOVED in the first release after LegacyBackupRemovalAfter.
//
// Until then every backup command keeps working — restore, list, download and
// dangerously-drop-all must, since existing archives have to stay usable —
// with three exceptions that stop NEW adoption without breaking any
// deployment's protection:
//   - every `oddk backup ...` command prints a notice to stderr naming its
//     replacement (the CLI; stdout and --json output are untouched);
//   - `backup setup-cron` refuses to create a NEW schedule (the daemon).
//     Existing schedules keep running and can still be edited, paused,
//     resumed or removed; `snapshot migrate-from-backups` moves them over;
//   - the README and CHANGELOG announce the removal.
//
// What removal will mean: the `backup` commands, the per-instance backup
// schedules and the backup_history catalogue. NOT the dump engine
// (BackupRDBMS / stageInstanceDump): `instance major-upgrade` takes a full
// backup as its rollback artifact, and logical snapshots stage through it.
const LegacyBackupRemovalAfter = "2026-12-31"

// LegacyBackupNewScheduleRefusal is the daemon's answer to a request for a new
// per-instance backup schedule.
const LegacyBackupNewScheduleRefusal = "per-instance backups are deprecated and will be removed in the first release after " +
	LegacyBackupRemovalAfter + ", so new backup schedules are no longer accepted. Schedule snapshots instead — " +
	"one schedule covers every instance: 'oddk snapshot setup-cron --utc-hour <hour>'. " +
	"Existing backup schedules keep running and can still be changed, paused, resumed or removed; " +
	"'oddk snapshot migrate-from-backups' moves them over"
