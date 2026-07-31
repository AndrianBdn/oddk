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
	ID                int              `db:"id" json:"-"`
	UTCHour           int              `db:"utc_hour" json:"utcHour"`
	IntervalHours     int              `db:"interval_hours" json:"intervalHours"`
	CleanupLocalDays  int              `db:"cleanup_local_days" json:"cleanupLocalDays"`
	CleanupRemoteDays int              `db:"cleanup_remote_days" json:"cleanupRemoteDays"`
	CreatedAt         rfc3339time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt         rfc3339time.Time `db:"updated_at" json:"updatedAt"`
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

	LocalLocation  sql.NullString `db:"local_location" json:"-"`
	RemoteLocation sql.NullString `db:"remote_location" json:"-"`
	Comment        sql.NullString `db:"comment" json:"-"`

	// Flattened for JSON output, mirroring BackupRecord.
	LocalPath  string `db:"-" json:"localLocation,omitempty"`
	RemotePath string `db:"-" json:"remoteLocation,omitempty"`
	CommentStr string `db:"-" json:"comment,omitempty"`
}
