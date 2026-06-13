package instances

import "github.com/hypersequent/oddk/internal/rfc3339time"

type RDBMSInstance struct {
	ID             int              `db:"id" json:"id,omitempty"`
	Name           string           `db:"name" json:"name"`
	Port           int              `db:"port" json:"port"`
	Version        string           `db:"version" json:"version"`
	Status         string           `db:"status" json:"status"`
	ContainerID    string           `db:"container_id" json:"containerId,omitempty"`
	Password       string           `db:"password" json:"password,omitempty"`
	CPUCores       int              `db:"cpu_cores" json:"cpuCores"`
	RAMMB          int              `db:"ram_mb" json:"ramMb"`
	ParameterGroup string           `db:"parameter_group" json:"parameterGroup"`
	Image          string           `db:"image" json:"image"`
	CreatedAt      rfc3339time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt      rfc3339time.Time `db:"updated_at" json:"updatedAt,omitzero"`
}
