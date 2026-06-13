package operations

import (
	"log"

	"github.com/hypersequent/oddk/internal/docker"
	"github.com/hypersequent/oddk/internal/store"
	"github.com/hypersequent/oddk/internal/store/instances"
)

// Dependencies holds all the dependencies needed by operations
type Dependencies struct {
	Store     *store.Store
	Docker    *docker.Client
	MasterKey []byte
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
