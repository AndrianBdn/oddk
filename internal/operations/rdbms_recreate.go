package operations

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
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

	prev := specFromInstance(instance, password, parameterGroup)
	next := prev
	next.Image = newImage
	next.Version = newVersion

	prevStatus := instance.Status
	if err := deps.Store.Instances.UpdateStatus(instance.Name, instances.StatusSwitching); err != nil {
		log.Printf("Error updating status to switching: %v", err)
	}

	rolledBack, err := applyClusterChange(ctx, deps, next, prev, instance.ContainerID)
	if rolledBack {
		return nil, fmt.Errorf("image %s could not start: %w (rolled back to the previous container image)", newImage, err)
	}
	if err != nil {
		if prevStatus != instances.StatusError && errors.Is(err, operr.ErrInvalid) {
			if statusErr := deps.Store.Instances.UpdateStatus(instance.Name, prevStatus); statusErr != nil {
				log.Printf("Error restoring status after preflight refusal: %v", statusErr)
			}
		}
		return nil, err
	}

	if err := deps.Store.Instances.UpdateImage(instance.Name, newImage, newVersion); err != nil {
		return nil, fmt.Errorf("record image: %w", err)
	}
	if err := deps.Store.Instances.UpdateStatus(instance.Name, instances.StatusRunning); err != nil {
		return nil, fmt.Errorf("record running status: %w", err)
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
