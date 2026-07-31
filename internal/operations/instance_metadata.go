package operations

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/store/parameters"
)

// instanceMetadataFile is the name of the instance configuration file stored at
// the root of a backup archive alongside globals.sql, databases/ and
// databases.json.
const instanceMetadataFile = "instance.json"

// InstanceMeta captures the instance's own configuration — everything needed to
// recreate the container it ran in, as opposed to the data inside it. Written
// at backup time and consumed when rebuilding an instance on a different host.
//
// Deliberately absent:
//
//   - The postgres password, in either form. The plaintext is never written to
//     disk, and the encrypted form lives in oddk.db, which a snapshot carries
//     separately. Putting it here would copy a credential into every
//     per-instance archive, including ones uploaded individually to S3.
//   - Status and container ID, which describe this host and mean nothing on
//     another one.
type InstanceMeta struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Image          string `json:"image"`
	Port           int    `json:"port"`
	CPUCores       int    `json:"cpuCores"`
	RAMMB          int    `json:"ramMb"`
	ParameterGroup string `json:"parameterGroup"`

	// ParameterGroupDefinition inlines the group rather than only naming it:
	// the target host has no reason to have a group by that name, and
	// recreating the container needs the actual parameters. Nil when the group
	// could not be read at backup time (see captureInstanceMetadata).
	ParameterGroupDefinition *parameters.ParameterGroup `json:"parameterGroupDefinition,omitempty"`
}

// captureInstanceMetadata builds the instance metadata from stored
// configuration. It needs no database connection, so it works regardless of
// cluster state.
//
// A parameter group that cannot be read is a warning, not an error: the group
// may have been deleted after the instance was created, and refusing to back up
// data over missing configuration would be the wrong trade. The group name is
// still recorded, so a later restore can report precisely what is missing.
func captureInstanceMetadata(deps *Dependencies, instance *instances.RDBMSInstance) *InstanceMeta {
	meta := &InstanceMeta{
		Name:           instance.Name,
		Version:        instance.Version,
		Image:          instance.Image,
		Port:           instance.Port,
		CPUCores:       instance.CPUCores,
		RAMMB:          instance.RAMMB,
		ParameterGroup: instance.ParameterGroup,
	}

	group, err := deps.Store.Parameters.GetGroup(instance.ParameterGroup)
	if err != nil {
		log.Printf("WARNING: backup %s: parameter group %q unreadable (%v); archive records the name but not its definition",
			instance.Name, instance.ParameterGroup, err)
		return meta
	}
	meta.ParameterGroupDefinition = group
	return meta
}

// writeInstanceMetadata writes the metadata as instance.json into dir (a backup
// staging directory that is about to be archived).
func writeInstanceMetadata(dir string, meta *InstanceMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal instance metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, instanceMetadataFile), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", instanceMetadataFile, err)
	}
	return nil
}

// readInstanceMetadata reads instance.json from an extracted backup directory.
// The bool is false (with nil error) when the file is absent — archives created
// before this metadata existed restore exactly as they did before, so callers
// must treat its absence as normal rather than as corruption.
func readInstanceMetadata(extractedDir string) (*InstanceMeta, bool, error) {
	path := filepath.Join(extractedDir, instanceMetadataFile)
	data, err := os.ReadFile(path) // #nosec G304 - path is the daemon's own extracted backup directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", instanceMetadataFile, err)
	}
	var meta InstanceMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", instanceMetadataFile, err)
	}
	return &meta, true, nil
}
