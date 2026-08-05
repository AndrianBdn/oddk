package snapshot

import (
	"database/sql"

	"github.com/andrianbdn/oddk/internal/rfc3339time"
)

// Plan is the deployment-wide snapshot schedule. There is at most one, enforced
// by `id INTEGER PRIMARY KEY CHECK (id = 1)` in migration 017 — a snapshot
// covers every instance, so a per-instance plan would express something that
// cannot happen.
type Plan struct {
	ID                int `db:"id" json:"-"`
	UTCHour           int `db:"utc_hour" json:"utcHour"`
	IntervalHours     int `db:"interval_hours" json:"intervalHours"`
	CleanupLocalDays  int `db:"cleanup_local_days" json:"cleanupLocalDays"`
	CleanupRemoteDays int `db:"cleanup_remote_days" json:"cleanupRemoteDays"`

	// Format is "physical" (pg_basebackup, the default) or "logical"
	// (portable pg_dump archives). Migration 018 backfills existing plans to
	// physical — that DEFAULT is the deliberate "binary by default" switch.
	Format string `db:"format" json:"format"`

	CreatedAt rfc3339time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt rfc3339time.Time `db:"updated_at" json:"updatedAt"`
}

// RunsAtHour reports whether the plan fires at the given UTC hour.
//
// The schedule is an interval ANCHORED to UTCHour, so it fires at every hour h
// where (h - UTCHour) is a whole number of intervals. IntervalHours is
// constrained to a divisor of 24 (see migration 017) precisely so this stays
// true across midnight.
func (p *Plan) RunsAtHour(hour int) bool {
	interval := p.IntervalHours
	if interval <= 0 {
		interval = 24 // defensive: a zero would divide by zero below
	}
	// Go's % keeps the sign of the dividend, so normalise into [0, interval).
	delta := ((hour-p.UTCHour)%interval + interval) % interval
	return delta == 0
}

// Record is one snapshot archive in the catalogue.
//
// It mirrors backup_history's dual-location model: a snapshot can exist locally,
// offsite, or both, and the schema CHECK forbids a row describing neither.
type Record struct {
	ID                int              `db:"id" json:"id,omitempty"`
	Filename          string           `db:"filename" json:"filename"`
	CreatedAt         rfc3339time.Time `db:"created_at" json:"createdAt"`
	Size              int64            `db:"size" json:"size"`
	Status            string           `db:"status" json:"status"`
	InstancesWithData int              `db:"instances_with_data" json:"instancesWithData"`
	ConfigOnly        int              `db:"config_only" json:"configOnly"`

	// Format is "physical" or "logical". Rows written before 0.1.61 default to
	// "logical" (migration 018) — every pre-physical archive is logical.
	Format string `db:"format" json:"format"`

	// InstancesJSON records which instances the archive holds and whether each
	// was captured with data (migration 019), as a JSON array of RecordInstance.
	// NULL on pre-019 rows — readers must treat that as "unknown", not "none".
	InstancesJSON sql.NullString `db:"instances_json" json:"-"`

	LocalLocation  sql.NullString `db:"local_location" json:"-"`
	RemoteLocation sql.NullString `db:"remote_location" json:"-"`
	Comment        sql.NullString `db:"comment" json:"-"`

	// Flattened for JSON output, mirroring BackupRecord. Instances is nil for a
	// pre-019 row (unknown coverage) and non-nil — possibly empty — once the
	// column is populated. Deliberately NOT omitempty: an empty deployment's
	// snapshot must serialize as "instances": [] (known empty), distinct from
	// null (pre-019, unknown) — omitempty would collapse the two, and the
	// contract says unknown must never be read as "no instances".
	LocalPath  string           `db:"-" json:"localLocation,omitempty"`
	RemotePath string           `db:"-" json:"remoteLocation,omitempty"`
	CommentStr string           `db:"-" json:"comment,omitempty"`
	Instances  []RecordInstance `db:"-" json:"instances"`
}

// RecordInstance is one instance's entry in a snapshot record: its name and
// whether the archive actually holds its data. HasData false means the entry
// is configuration-only — a restore of that instance from this archive would
// produce no database contents.
type RecordInstance struct {
	Name    string `json:"name"`
	HasData bool   `json:"hasData"`
}
