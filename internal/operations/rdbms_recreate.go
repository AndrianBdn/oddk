package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/store/instances"
)

// recreateInstanceOnImage stops and recreates the instance's container on
// newImage/newVersion (reusing the existing data volume), starts it, waits for
// PostgreSQL readiness, and updates the store. It is shared by the switch and
// update operations.
//
// The caller must have already validated that newImage is present locally and
// that newVersion is the same PostgreSQL major as the instance's current
// version (a different major cannot start on the existing data dir).
func recreateInstanceOnImage(ctx context.Context, deps *Dependencies, instance *instances.RDBMSInstance, newImage, newVersion string) (*instances.RDBMSInstance, error) {
	password, err := crypto.DecryptPassword(instance.Password, deps.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt password: %w", err)
	}

	parameterGroup, err := deps.Store.Parameters.GetGroup(instance.ParameterGroup)
	if err != nil {
		return nil, fmt.Errorf("get parameter group %s: %w", instance.ParameterGroup, err)
	}

	if err := deps.Store.Instances.UpdateStatus(instance.Name, instances.StatusSwitching); err != nil {
		log.Printf("Error updating status to switching: %v", err)
	}

	newContainerID, err := deps.Docker.RecreateContainer(
		instance.Name,
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
		if statusErr := deps.Store.Instances.UpdateStatus(instance.Name, instances.StatusError); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return nil, fmt.Errorf("recreate container: %w", err)
	}

	if err := deps.Store.Instances.UpdateContainerID(instance.Name, newContainerID); err != nil {
		log.Printf("Error updating container ID: %v", err)
	}
	if err := deps.Store.Instances.UpdateImage(instance.Name, newImage, newVersion); err != nil {
		log.Printf("Error updating image: %v", err)
	}

	if err := deps.Docker.StartContainer(newContainerID); err != nil {
		if statusErr := deps.Store.Instances.UpdateStatus(instance.Name, instances.StatusError); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return nil, fmt.Errorf("start container: %w", err)
	}

	// Docker reporting the container as started does not mean PostgreSQL is
	// ready to accept connections; wait for it before reporting "running".
	if err := waitForPostgresReady(ctx, instance.Port, password); err != nil {
		if statusErr := deps.Store.Instances.UpdateStatus(instance.Name, instances.StatusError); statusErr != nil {
			log.Printf("Error updating status to error: %v", statusErr)
		}
		return nil, fmt.Errorf("wait for PostgreSQL readiness: %w", err)
	}

	if err := deps.Store.Instances.UpdateStatus(instance.Name, instances.StatusRunning); err != nil {
		log.Printf("Error updating status to running: %v", err)
	}

	updated, err := deps.Store.Instances.Get(instance.Name)
	if err != nil {
		return nil, fmt.Errorf("get updated instance: %w", err)
	}
	return updated, nil
}

// imageDiffersFromContainer reports whether the local image tag resolves to a
// different image ID than the one the container is currently running — i.e. a
// re-pulled patch is waiting to be adopted. On any uncertainty (image or
// container not inspectable) it returns true so the caller recreates, which is
// the safe default.
func imageDiffersFromContainer(deps *Dependencies, image, containerID string) bool {
	if containerID == "" {
		return true
	}
	tagID, ok := deps.Docker.GetImageID(image)
	if !ok {
		return true
	}
	containerImageID, err := deps.Docker.GetContainerImageID(containerID)
	if err != nil {
		return true
	}
	return tagID != containerImageID
}
