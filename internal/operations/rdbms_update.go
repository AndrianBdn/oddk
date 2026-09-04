package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"github.com/moby/moby/api/types/jsonstream"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
)

// UpdateRDBMSOp re-pulls the image for an instance's current tag and recreates
// the container if the pull produced a newer image (e.g. a patch release with
// security fixes). It is the one-step equivalent of `pull` followed by
// `instance switch` onto the same tag. Like pull, it streams raw Docker
// progress frames to a writer so the CLI can render live progress; it appends a
// final status frame describing the outcome.
type UpdateRDBMSOp struct {
	deps     *Dependencies
	params   UpdateRDBMSParams
	progress io.Writer
	result   *SwitchRDBMSResult
}

// NewUpdateRDBMSOp creates a new update RDBMS operation.
func NewUpdateRDBMSOp(deps *Dependencies, params UpdateRDBMSParams) *UpdateRDBMSOp {
	return &UpdateRDBMSOp{
		deps:   deps,
		params: params,
	}
}

func (op *UpdateRDBMSOp) Name() string {
	return fmt.Sprintf("UpdateRDBMS[%s]", op.params.Name)
}

func (op *UpdateRDBMSOp) Type() OpType {
	return OpTypeWrite
}

// SetProgressWriter sets the destination for raw Docker pull progress frames
// and the final status frame. Call before Execute. If unset, progress is
// discarded.
func (op *UpdateRDBMSOp) SetProgressWriter(w io.Writer) {
	op.progress = w
}

func (op *UpdateRDBMSOp) Execute(ctx context.Context) error {
	progress := op.progress
	if progress == nil {
		progress = io.Discard
	}

	instance, err := op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	// Target the instance's current image by default; an explicit override lets
	// update double as a same-major pull+switch.
	targetImage := op.params.Image
	if targetImage == "" {
		targetImage = instance.Image
	}

	targetVersion := instance.Version
	if detected, ok := docker.DetectPGVersionFromImage(targetImage); ok {
		targetVersion = detected
	}

	// Same-major guard: update reuses the data volume, so it cannot cross a
	// major version. Direct the user to major-upgrade instead.
	curMajor, curOK := parseMajorVersion(instance.Version)
	newMajor, newOK := parseMajorVersion(targetVersion)
	if curOK && newOK && newMajor != curMajor {
		return operr.Invalidf(
			"cannot update instance from PostgreSQL %d to %d: 'update' stays within the same major version; use 'oddk instance major-upgrade %s --target-version %d'",
			curMajor, newMajor, op.params.Name, newMajor,
		)
	}

	// Re-pull the tag (streaming progress). A moving tag fetches the newest
	// patch; Docker reports "up to date" cheaply when nothing changed.
	if err := op.deps.Docker.PullImageProgress(ctx, targetImage, progress); err != nil {
		return err
	}

	// Recreate only when there is actually something new to adopt: a different
	// image/version than recorded, or the same tag now resolving to a newer
	// local image than the running container.
	needsRecreate := targetImage != instance.Image ||
		targetVersion != instance.Version ||
		imageDiffersFromContainer(op.deps, targetImage, instance.ContainerID)

	if !needsRecreate {
		emitStatus(progress, "Instance %s is already up to date (image %s)", op.params.Name, targetImage)
		op.result = &SwitchRDBMSResult{Instance: instance}
		return nil
	}

	updated, err := recreateInstanceOnImage(ctx, op.deps, instance, targetImage, targetVersion)
	if err != nil {
		return err
	}

	emitStatus(progress, "Instance %s updated to image %s (version %s)", op.params.Name, updated.Image, updated.Version)
	op.result = &SwitchRDBMSResult{Instance: updated}
	return nil
}

// GetResult returns the operation result.
func (op *UpdateRDBMSOp) GetResult() *SwitchRDBMSResult {
	return op.result
}

// emitStatus writes a Docker-style status frame to w so the CLI's progress
// renderer prints it as a plain status line.
func emitStatus(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(jsonstream.Message{Status: fmt.Sprintf(format, args...)}); err != nil {
		log.Printf("emit update status: %v", err)
	}
}
