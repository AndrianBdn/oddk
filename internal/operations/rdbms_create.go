package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/hypersequent/oddk/internal/crypto"
	"github.com/hypersequent/oddk/internal/docker"
	"github.com/hypersequent/oddk/internal/operr"
	"github.com/hypersequent/oddk/internal/util"
)

// CreateRDBMSOp creates a new RDBMS instance
type CreateRDBMSOp struct {
	deps   *Dependencies
	params CreateRDBMSParams
	result *CreateRDBMSResult
}

// NewCreateRDBMSOp creates a new create RDBMS operation
func NewCreateRDBMSOp(deps *Dependencies, params CreateRDBMSParams) *CreateRDBMSOp {
	return &CreateRDBMSOp{
		deps:   deps,
		params: params,
	}
}

func (op *CreateRDBMSOp) Name() string {
	return fmt.Sprintf("CreateRDBMS[%s]", op.params.Name)
}

func (op *CreateRDBMSOp) Type() OpType {
	return OpTypeWrite
}

func (op *CreateRDBMSOp) Execute(ctx context.Context) error {
	// Set defaults
	if op.params.Version == "" {
		op.params.Version = "17"
	}
	if op.params.Port == 0 {
		op.params.Port = 5432
	}

	// Derive image name
	image := op.params.Image
	if image == "" {
		image = fmt.Sprintf("postgres:%s", op.params.Version)
	}

	// Validate version/image consistency when both are provided
	if op.params.Image != "" {
		if detectedVersion, ok := docker.DetectPGVersionFromImage(image); ok {
			if detectedVersion != op.params.Version {
				return operr.Invalidf("image tag suggests PostgreSQL %s but --version %s was specified", detectedVersion, op.params.Version)
			}
		}
	}

	// Check if name is already in use
	nameInUse, err := op.deps.Store.Instances.IsNameInUse(op.params.Name)
	if err != nil {
		return fmt.Errorf("check name availability: %w", err)
	}
	if nameInUse {
		return operr.Conflictf("instance name already exists: %s", op.params.Name)
	}

	// Check if port is already in use
	portInUse, existingInstance, err := op.deps.Store.Instances.IsPortInUse(op.params.Port)
	if err != nil {
		return fmt.Errorf("check port availability: %w", err)
	}
	if portInUse {
		return operr.Conflictf("port %d is already in use by instance: %s", op.params.Port, existingInstance)
	}

	// Generate secure password
	password := util.GenerateSecurePassword(24)

	// Encrypt password for storage
	encryptedPassword, err := crypto.EncryptPassword(password, op.deps.MasterKey)
	if err != nil {
		return fmt.Errorf("encrypt password: %w", err)
	}

	instance, err := op.deps.Store.Instances.Create(
		op.params.Name,
		op.params.Port,
		op.params.Version,
		encryptedPassword,
		"", // Container ID will be updated later
		op.params.CPUCores,
		op.params.RAMMB,
		op.params.ParameterGroup,
		image,
	)
	if err != nil {
		return fmt.Errorf("create instance in store: %w", err)
	}

	parameterGroup, err := op.deps.Store.Parameters.GetGroup(op.params.ParameterGroup)
	if err != nil {
		if delErr := op.deps.Store.Instances.Delete(op.params.Name); delErr != nil {
			log.Printf("Error cleaning up instance after parameter group error: %v", delErr)
		}
		return operr.Invalidf("get parameter group %s: %w", op.params.ParameterGroup, err)
	}

	containerID, err := op.deps.Docker.CreateContainer(
		op.params.Name,
		op.params.Version,
		image,
		op.params.Port,
		password,
		op.params.CPUCores,
		op.params.RAMMB,
		op.params.ParameterGroup,
		parameterGroup.Parameters,
	)
	if err != nil {
		if delErr := op.deps.Store.Instances.Delete(op.params.Name); delErr != nil {
			log.Printf("Error cleaning up instance after container creation failure: %v", delErr)
		}
		return fmt.Errorf("create container: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateContainerID(op.params.Name, containerID); err != nil {
		log.Printf("Error updating container ID: %v", err)
		// Continue anyway since container was created successfully
	}

	if err := op.deps.Docker.StartContainer(containerID); err != nil {
		op.cleanupFailedCreate(containerID)
		return fmt.Errorf("start container: %w", err)
	}

	// Wait for PostgreSQL to actually accept connections before reporting the
	// instance as running. Docker reporting the container as started does not
	// mean the server is ready (first boot runs initdb), and reporting "running"
	// early surfaced a transient "port is not accessible" to clients that
	// connected immediately after create.
	if err := waitForPostgresReady(ctx, op.params.Port, password); err != nil {
		op.cleanupFailedCreate(containerID)
		return fmt.Errorf("wait for PostgreSQL readiness: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, "running"); err != nil {
		log.Printf("Error updating RDBMS status to running: %v", err)
	}

	instance.ContainerID = containerID
	instance.Status = "running"

	// Store result
	op.result = &CreateRDBMSResult{
		Instance: instance,
		Password: password,
	}

	return nil
}

// cleanupFailedCreate tears down a partially-created instance after the
// container started but never became usable (start error or readiness timeout).
// It removes the container, the freshly-created data volume, and the store row
// so a same-name retry isn't blocked by an orphan volume. The volume is safe to
// remove here because CreateContainer refuses pre-existing volumes, so this
// volume was created by this operation. Volume deletion deliberately stays out
// of Docker.StartContainer, which also starts existing instances whose volumes
// hold real data.
func (op *CreateRDBMSOp) cleanupFailedCreate(containerID string) {
	if err := op.deps.Docker.RemoveContainer(containerID); err != nil {
		log.Printf("Error removing container after failed create: %v", err)
	}
	if err := op.deps.Docker.RemoveVolume(fmt.Sprintf("oddk-data-%s", op.params.Name)); err != nil {
		log.Printf("Error removing volume after failed create: %v", err)
	}
	if err := op.deps.Store.Instances.Delete(op.params.Name); err != nil {
		log.Printf("Error cleaning up instance after failed create: %v", err)
	}
}

// GetResult returns the operation result
func (op *CreateRDBMSOp) GetResult() *CreateRDBMSResult {
	return op.result
}
