package operations

import (
	"context"
	"fmt"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
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
			curMajor, newMajor, op.params.Name, newMajor,
		)
	}

	_, exists := op.deps.Docker.CheckImageExists(newImage)
	if !exists {
		return operr.Invalidf("image not found locally. Please run 'oddk pull --image %s' first", newImage)
	}

	// If neither the recorded tag nor the version is changing, recreating is
	// only worthwhile when the tag now resolves to a newer local image than the
	// running container (e.g. a re-pulled patch release). Otherwise there is
	// genuinely nothing to do. Run 'oddk pull' first, or 'oddk instance update',
	// to fetch a newer patch for a moving tag.
	if newImage == instance.Image && newVersion == instance.Version &&
		!imageDiffersFromContainer(op.deps, newImage, instance.ContainerID) {
		return operr.Invalidf("instance already uses image %s with version %s, and it is up to date", newImage, newVersion)
	}

	updated, err := recreateInstanceOnImage(ctx, op.deps, instance, newImage, newVersion)
	if err != nil {
		return err
	}

	op.result = &SwitchRDBMSResult{
		Instance: updated,
	}

	return nil
}

// GetResult returns the operation result
func (op *SwitchRDBMSOp) GetResult() *SwitchRDBMSResult {
	return op.result
}
