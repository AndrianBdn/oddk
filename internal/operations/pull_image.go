package operations

import (
	"context"
	"fmt"
	"io"
	"log"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
)

// PullImageOp pulls a PostgreSQL image from Docker Hub
type PullImageOp struct {
	deps     *Dependencies
	params   PullImageParams
	progress io.Writer // where raw Docker pull progress is streamed (nil = discard)
	image    string    // resolved image name (set by Resolve)
	result   *PullImageResult
}

// NewPullImageOp creates a new pull image operation
func NewPullImageOp(deps *Dependencies, params PullImageParams) *PullImageOp {
	return &PullImageOp{
		deps:   deps,
		params: params,
	}
}

func (op *PullImageOp) Name() string {
	if op.params.Image != "" {
		return fmt.Sprintf("PullImage[%s]", op.params.Image)
	}
	return fmt.Sprintf("PullImage[postgres:%s]", op.params.Version)
}

func (op *PullImageOp) Type() OpType {
	return OpTypeWrite
}

// SetProgressWriter sets the destination for raw Docker pull progress frames.
// Call before Execute. If unset, progress is discarded.
func (op *PullImageOp) SetProgressWriter(w io.Writer) {
	op.progress = w
}

// Resolve computes the target image name and validates version/image
// consistency. It is a cheap, side-effect-free pre-flight so the HTTP handler
// can return a clean 4xx before committing to a streaming response. Execute
// calls it implicitly if it hasn't run yet.
func (op *PullImageOp) Resolve() error {
	if err := validateVersionShape(op.params.Version); err != nil {
		return err
	}
	imageName := op.params.Image
	if imageName == "" {
		if op.params.Version == "" {
			return operr.Invalidf("version or image is required")
		}
		imageName = fmt.Sprintf("postgres:%s", op.params.Version)
	}

	// Validate version/image consistency when both are provided.
	if op.params.Version != "" && op.params.Image != "" {
		if detectedVersion, ok := docker.DetectPGVersionFromImage(imageName); ok {
			// Majors, not strings: --version 17.2 with postgres:17 agrees.
			detectedMajor, _ := parseMajorVersion(detectedVersion)
			if wantMajor, _ := parseMajorVersion(op.params.Version); detectedMajor != wantMajor {
				return operr.Invalidf("image tag suggests PostgreSQL %s but --version %s was specified", detectedVersion, op.params.Version)
			}
		}
	}

	op.image = imageName
	return nil
}

func (op *PullImageOp) Execute(ctx context.Context) error {
	if op.image == "" {
		if err := op.Resolve(); err != nil {
			return err
		}
	}

	progress := op.progress
	if progress == nil {
		progress = io.Discard
	}

	// IfMissing (used by create/switch auto-provisioning) short-circuits when
	// the image is already present, so a cached image is reused without a
	// network round-trip. A standalone pull/update leaves IfMissing false so a
	// moving tag is always re-pulled.
	if op.params.IfMissing {
		if tags, ok := op.deps.Docker.CheckImageExists(op.image); ok {
			log.Printf("Image %s already present locally; skipping pull", op.image)
			emitStatus(progress, "Image %s already present locally", op.image)
			op.result = &PullImageResult{
				Version: op.params.Version,
				Tags:    tags,
				Message: fmt.Sprintf("Image %s already present locally", op.image),
			}
			return nil
		}
	}

	// Otherwise always perform the pull rather than short-circuiting when the
	// image is already present locally: a moving tag (e.g. "postgres:17") must
	// be re-pulled to fetch the latest published patch release. Docker reports
	// "Image is up to date" cheaply when nothing changed.
	log.Printf("Pulling image %s from Docker Hub...", op.image)
	if err := op.deps.Docker.PullImageProgress(ctx, op.image, progress); err != nil {
		return err
	}

	tags, _ := op.deps.Docker.CheckImageExists(op.image)
	op.result = &PullImageResult{
		Version: op.params.Version,
		Tags:    tags,
		Message: fmt.Sprintf("Successfully pulled %s", op.image),
	}
	return nil
}

// GetResult returns the operation result
func (op *PullImageOp) GetResult() *PullImageResult {
	return op.result
}
