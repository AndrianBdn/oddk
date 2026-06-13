package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/hypersequent/oddk/internal/crypto"
	"github.com/hypersequent/oddk/internal/docker"
	"github.com/hypersequent/oddk/internal/operr"
)

// SwitchRDBMSOp switches an existing RDBMS instance to a different Docker image
type SwitchRDBMSOp struct {
	deps   *Dependencies
	params SwitchRDBMSParams
	result *SwitchRDBMSResult
}

// NewSwitchRDBMSOp creates a new switch RDBMS operation
func NewSwitchRDBMSOp(deps *Dependencies, params SwitchRDBMSParams) *SwitchRDBMSOp {
	return &SwitchRDBMSOp{
		deps:   deps,
		params: params,
	}
}

func (op *SwitchRDBMSOp) Name() string {
	return fmt.Sprintf("SwitchRDBMS[%s]", op.params.Name)
}

func (op *SwitchRDBMSOp) Type() OpType {
	return OpTypeWrite
}

func (op *SwitchRDBMSOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	// Resolve new version: use provided, auto-detect from image tag, or keep current
	newVersion := op.params.Version
	if newVersion == "" {
		if detectedVersion, ok := docker.DetectPGVersionFromImage(op.params.Image); ok {
			newVersion = detectedVersion
		} else {
			newVersion = instance.Version
		}
	}

	newImage := op.params.Image

	// Check if anything would change
	if newImage == instance.Image && newVersion == instance.Version {
		return operr.Invalidf("instance already uses image %s with version %s", newImage, newVersion)
	}

	if detectedVersion, ok := docker.DetectPGVersionFromImage(newImage); ok {
		if detectedVersion != newVersion {
			return operr.Invalidf("image tag suggests PostgreSQL %s but --version %s was specified", detectedVersion, newVersion)
		}
	}

	// Reject cross-major switches before touching the container. 'switch' reuses
	// the existing data volume, so a different major would make the new server
	// refuse to start on the old major's data dir (and PG18+ also moves the data
	// mount target). Major-version changes must go through major-upgrade, which
	// migrates data via dump/restore. Returning here leaves the instance running
	// and untouched.
	curMajor, curOK := parseMajorVersion(instance.Version)
	newMajor, newOK := parseMajorVersion(newVersion)
	if curOK && newOK && newMajor != curMajor {
		return operr.Invalidf(
			"cannot switch instance from PostgreSQL %d to %d: 'switch' only changes the image within the same major version; use 'oddk instance major-upgrade %s --target-version %d' for a major-version upgrade",
			curMajor, newMajor, op.params.Name, newMajor)
	}

	_, exists := op.deps.Docker.CheckImageExists(newImage)
	if !exists {
		return operr.Invalidf("image not found locally. Please run 'oddk pull --image %s' first", newImage)
	}

	// Decrypt password for container recreation
	password, err := crypto.DecryptPassword(instance.Password, op.deps.MasterKey)
	if err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}

	parameterGroup, err := op.deps.Store.Parameters.GetGroup(instance.ParameterGroup)
	if err != nil {
		return fmt.Errorf("get parameter group %s: %w", instance.ParameterGroup, err)
	}

	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, "switching"); err != nil {
		log.Printf("Error updating status to switching: %v", err)
	}

	// Recreate the container with the new image
	newContainerID, err := op.deps.Docker.RecreateContainer(
		op.params.Name,
		newVersion,
		newImage,
		instance.Port,
		password,
		instance.CPUCores,
		instance.RAMMB,
		instance.ParameterGroup,
		parameterGroup.Parameters,
		instance.ContainerID,
	)
	if err != nil {
		if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, "error"); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return fmt.Errorf("recreate container: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateContainerID(op.params.Name, newContainerID); err != nil {
		log.Printf("Error updating container ID: %v", err)
	}

	if err := op.deps.Store.Instances.UpdateImage(op.params.Name, newImage, newVersion); err != nil {
		log.Printf("Error updating image: %v", err)
	}

	if err := op.deps.Docker.StartContainer(newContainerID); err != nil {
		if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, "error"); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return fmt.Errorf("start container: %w", err)
	}

	// Wait for the recreated container to actually accept connections before
	// reporting "running"; Docker reporting the container as started does not
	// mean PostgreSQL is ready.
	if err := waitForPostgresReady(ctx, instance.Port, password); err != nil {
		if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, "error"); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return fmt.Errorf("wait for PostgreSQL readiness: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, "running"); err != nil {
		log.Printf("Error updating status to running: %v", err)
	}

	instance, err = op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get updated instance: %w", err)
	}

	op.result = &SwitchRDBMSResult{
		Instance: instance,
	}

	return nil
}

// GetResult returns the operation result
func (op *SwitchRDBMSOp) GetResult() *SwitchRDBMSResult {
	return op.result
}
