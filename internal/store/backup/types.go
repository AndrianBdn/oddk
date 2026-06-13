package backup

import (
	"database/sql"

	"github.com/hypersequent/oddk/internal/rfc3339time"
)

type BackupRecord struct {
	ID             int              `db:"id" json:"id,omitempty"`
	InstanceName   string           `db:"instance_name" json:"instanceName"`
	Timestamp      rfc3339time.Time `db:"timestamp" json:"timestamp"`
	Size           int64            `db:"size" json:"size"`
	LocalLocation  sql.NullString   `db:"local_location" json:"-"`
	RemoteLocation sql.NullString   `db:"remote_location" json:"-"`
	LocalPath      string           `db:"-" json:"localLocation,omitempty"`  // For JSON output
	RemotePath     string           `db:"-" json:"remoteLocation,omitempty"` // For JSON output
	Status         string           `db:"status" json:"status"`
	Comment        sql.NullString   `db:"comment" json:"-"`
	CommentStr     string           `db:"-" json:"comment,omitempty"` // For JSON output
	CreatedAt      rfc3339time.Time `db:"created_at" json:"createdAt"`
	// Computed fields from filesystem check (not stored in DB)
	FileExists bool  `db:"-" json:"fileExists,omitempty"`
	ActualSize int64 `db:"-" json:"actualSize,omitempty"`
}
