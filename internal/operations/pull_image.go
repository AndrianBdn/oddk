package operations

import (
	"context"
	"fmt"
	"log"

	"github.com/hypersequent/oddk/internal/docker"
	"github.com/hypersequent/oddk/internal/operr"
)

// PullImageOp pulls a PostgreSQL image from Docker Hub
type PullImageOp struct {
	deps   *Dependencies
	params PullImageParams
	result *PullImageResult
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

func (op *PullImageOp) Execute(ctx context.Context) error {
	// Compute image name
	imageName := op.params.Image
	if imageName == "" {
		if op.params.Version == "" {
			op.params.Version = "17"
		}
		imageName = fmt.Sprintf("postgres:%s", op.params.Version)
	}

	// Validate version/image consistency when both are provided
	if op.params.Version != "" && op.params.Image != "" {
		if detectedVersion, ok := docker.DetectPGVersionFromImage(imageName); ok {
			if detectedVersion != op.params.Version {
				return operr.Invalidf("image tag suggests PostgreSQL %s but --version %s was specified", detectedVersion, op.params.Version)
			}
		}
	}

	// First, check if image already exists locally
	existingTags, imageExists := op.deps.Docker.CheckImageExists(imageName)

	if imageExists {
		log.Printf("Image %s already exists locally with tags: %v", imageName, existingTags)
		op.result = &PullImageResult{
			Version: op.params.Version,
			Tags:    existingTags,
			Message: fmt.Sprintf("Image %s already exists locally", imageName),
		}
		return nil
	}

	// Pull the image
	log.Printf("Pulling image %s from Docker Hub...", imageName)
	if err := op.deps.Docker.PullImage(imageName); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	tags, _ := op.deps.Docker.CheckImageExists(imageName)

	op.result = &PullImageResult{
		Version: op.params.Version,
		Tags:    tags,
		Message: fmt.Sprintf("Successfully pulled %s", imageName),
	}

	return nil
}

// GetResult returns the operation result
func (op *PullImageOp) GetResult() *PullImageResult {
	return op.result
}
