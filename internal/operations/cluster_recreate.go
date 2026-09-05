package operations

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/store/parameters"
)

// clusterSpec is the container shape RecreateContainer needs. prev and next
// of applyClusterChange differ in image and/or parameter group.
type clusterSpec struct {
	Name           string
	Version        string
	Image          string
	Port           int
	Password       string
	CPUCores       int
	RAMMB          int
	ParameterGroup string
	Parameters     []parameters.Parameter
}

func specFromInstance(inst *instances.RDBMSInstance, password string, group *parameters.ParameterGroup) clusterSpec {
	return clusterSpec{
		Name:           inst.Name,
		Version:        inst.Version,
		Image:          inst.Image,
		Port:           inst.Port,
		Password:       password,
		CPUCores:       inst.CPUCores,
		RAMMB:          inst.RAMMB,
		ParameterGroup: inst.ParameterGroup,
		Parameters:     group.Parameters,
	}
}

// classifyRecreateError tags the refusals a client can act on (an image or a
// parameter group that cannot work for this instance) as ErrInvalid -> HTTP 400.
// Anything else is returned unchanged and maps to 500.
func classifyRecreateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, parameters.ErrSharedMemoryTooLarge) || errors.Is(err, docker.ErrPreflight) {
		return operr.Invalidf("%w", err)
	}
	return err
}

// recreateStartReady replaces the instance container with spec and waits until
// PostgreSQL accepts connections. On start/readiness failure the new container
// is Stopped — unless-stopped would otherwise crash-loop a config that can
// never boot. The returned id is set whenever RecreateContainer succeeded,
// even when err != nil, so the caller can roll back against that id.
func recreateStartReady(ctx context.Context, deps *Dependencies, spec clusterSpec, oldContainerID string) (string, error) {
	id, err := deps.Docker.RecreateContainer(
		spec.Name, spec.Version, spec.Image, spec.Port,
		spec.CPUCores, spec.RAMMB, spec.ParameterGroup, spec.Parameters,
		oldContainerID,
	)
	if err != nil {
		return "", err
	}
	if err := deps.Docker.StartContainer(id); err != nil {
		stopFailedContainer(deps, id)
		return id, fmt.Errorf("start container: %w", err)
	}
	if err := waitForPostgresReady(ctx, spec.Port, spec.Password); err != nil {
		stopFailedContainer(deps, id)
		return id, fmt.Errorf("wait for PostgreSQL readiness: %w", annotateReadyError(deps, id, err))
	}
	return id, nil
}

func stopFailedContainer(deps *Dependencies, id string) {
	if id == "" {
		return
	}
	if err := deps.Docker.StopContainer(id); err != nil {
		log.Printf("Warning: could not stop container %s after a failed start/readiness wait: %v", id, err)
	}
}

func annotateReadyError(deps *Dependencies, containerID string, readyErr error) error {
	if containerID == "" || deps == nil || deps.Docker == nil {
		return readyErr
	}
	logs, err := deps.Docker.GetContainerLogs(containerID, "80")
	if err != nil || strings.TrimSpace(logs) == "" {
		return readyErr
	}
	return fmt.Errorf("%w\ncontainer logs:\n%s", readyErr, logs)
}

// applyClusterChange recreates onto next. Failures that wrap docker.ErrPreflight
// leave the previous container running and do not roll back. Any other failure
// (Recreate after destroy, start, readiness) stops the new container and
// recreates prev. On rollback success the store points at the restored
// container, status is running, and the original error is returned with
// rolledBack=true.
//
// No container id is returned: every store write this function is responsible
// for has already happened by the time it returns.
func applyClusterChange(ctx context.Context, deps *Dependencies, next, prev clusterSpec, oldContainerID string) (rolledBack bool, err error) {
	// A pull may already have moved prev.Image to the new patch. Pin the
	// image the old container actually used before removing it. Try its name
	// too, since a previous crash may have left the stored ID stale.
	for _, ref := range []string{oldContainerID, docker.ContainerName(prev.Name)} {
		if ref == "" {
			continue
		}
		imageID, inspectErr := deps.Docker.GetContainerImageID(ref)
		if cerrdefs.IsNotFound(inspectErr) {
			continue
		}
		if inspectErr != nil {
			return false, classifyRecreateError(fmt.Errorf("%w: resolve rollback image: %w", docker.ErrPreflight, inspectErr))
		}
		if _, exists := deps.Docker.CheckImageExists(imageID); !exists {
			return false, classifyRecreateError(fmt.Errorf("%w: previous image %s is unavailable for rollback", docker.ErrPreflight, imageID))
		}
		prev.Image = imageID
		break
	}

	id, err := recreateStartReady(ctx, deps, next, oldContainerID)
	if err != nil {
		if errors.Is(err, docker.ErrPreflight) {
			return false, classifyRecreateError(err)
		}
		rbID, rbErr := recreateStartReady(ctx, deps, prev, id)
		if rbErr != nil {
			if statusErr := deps.Store.Instances.UpdateStatus(next.Name, instances.StatusError); statusErr != nil {
				log.Printf("Error updating status to error: %v", statusErr)
			}
			// Point the store at whichever container actually exists. The
			// rollback's RecreateContainer removes id before creating rbID, so
			// naming id once rbID exists would record a container that is gone.
			// rbID is empty only when the rollback failed before creating one,
			// and then id is either still there (a preflight refusal) or gone
			// too — harmless, since both destroy and the next recreate also
			// clean up by container NAME.
			if orphan := rbID; orphan != "" || id != "" {
				if orphan == "" {
					orphan = id
				}
				if persistErr := deps.Store.Instances.UpdateContainerID(next.Name, orphan); persistErr != nil {
					log.Printf("Error recording container id %s after a failed rollback: %v", orphan, persistErr)
				}
			}
			return false, fmt.Errorf("%w; rollback to the previous configuration also failed: %w", err, rbErr)
		}
		if persistErr := deps.Store.Instances.UpdateContainerID(next.Name, rbID); persistErr != nil {
			if statusErr := deps.Store.Instances.UpdateStatus(next.Name, instances.StatusError); statusErr != nil {
				log.Printf("Error updating status to error: %v", statusErr)
			}
			return true, fmt.Errorf("%w (rolled back the container, but failed to record its id: %w)", err, persistErr)
		}
		if statusErr := deps.Store.Instances.UpdateStatus(next.Name, instances.StatusRunning); statusErr != nil {
			log.Printf("Error updating status to running after rollback: %v", statusErr)
		}
		return true, err
	}
	if err := deps.Store.Instances.UpdateContainerID(next.Name, id); err != nil {
		return false, fmt.Errorf("record container id: %w", err)
	}
	return false, nil
}
