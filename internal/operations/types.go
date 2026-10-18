package operations

import (
	"log"

	"github.com/andrianbdn/oddk/internal/compression"
	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/store/instances"
)

// Dependencies holds all the dependencies needed by operations
type Dependencies struct {
	Store     *store.Store
	Docker    *docker.Client
	MasterKey []byte
	// Archives writes and reads this host's archives, encrypted to the key
	// derived from MasterKey (ArchiveCompressor). Built once, with MasterKey.
	Archives  *compression.Compressor
	DataDir   string // Added for backup operations
	BackupDir string // Added for backup operations
	Logger    *log.Logger
}

// CreateRDBMSParams contains parameters for creating an RDBMS instance
type CreateRDBMSParams struct {
	Name           string
	Version        string
	Image          string
	Port           int
	CPUCores       int
	RAMMB          int
	ParameterGroup string
}

// CreateRDBMSResult contains the result of creating an RDBMS instance
type CreateRDBMSResult struct {
	Instance *instances.RDBMSInstance
	Password string // Plaintext password for initial response
}

// UpdateStateParams contains parameters for updating instance state
type UpdateStateParams struct {
	Name  string
	State string // "start" or "stop"
}

// DeleteRDBMSParams contains parameters for deleting an RDBMS instance
type DeleteRDBMSParams struct {
	Name string
}

// PullImageParams contains parameters for pulling a Docker image
type PullImageParams struct {
	Version string
	Image   string
	// IfMissing pulls only when the image is not already present locally;
	// when present it is a no-op (no network). Used by create/switch to
	// auto-provision an image without forcing a re-pull of a cached one.
	// Standalone pull and update leave this false so a moving tag is refreshed.
	IfMissing bool
}

// PullImageResult contains the result of pulling a Docker image
type PullImageResult struct {
	Version string
	Tags    []string
	Message string
}

// SwitchRDBMSParams contains parameters for switching an instance's image
type SwitchRDBMSParams struct {
	Name    string
	Image   string
	Version string // empty = keep current instance version
}

// SwitchRDBMSResult contains the result of switching an instance's image
type SwitchRDBMSResult struct {
	Instance *instances.RDBMSInstance
}

// UpdateRDBMSParams contains parameters for updating an instance to the latest
// image for its tag (re-pull + recreate if a newer patch is available).
type UpdateRDBMSParams struct {
	Name  string
	Image string // empty = the instance's current image
}

// UpgradeRDBMSParams contains parameters for a major-version upgrade of an instance
type UpgradeRDBMSParams struct {
	Name          string
	TargetVersion string // target PostgreSQL major version, e.g. "18"
	Image         string // optional target image; required for custom (non-postgres:) images
}

// UpgradeRDBMSResult contains the result of a major-version upgrade
type UpgradeRDBMSResult struct {
	Instance          *instances.RDBMSInstance `json:"instance"`
	FromVersion       string                   `json:"fromVersion"`
	ToVersion         string                   `json:"toVersion"`
	BackupID          int                      `json:"backupId"`
	DatabasesRestored int                      `json:"databasesRestored"`
}

// ParameterGroupGetParams contains parameters for getting a parameter group
type ParameterGroupGetParams struct {
	Name string
}

// ParameterGroupPutParams contains parameters for creating a parameter group
type ParameterGroupPutParams struct {
	Name       string
	Parameters []byte // JSON data
}

// ParameterGroupDeleteParams contains parameters for deleting a parameter group
type ParameterGroupDeleteParams struct {
	Name string
}
