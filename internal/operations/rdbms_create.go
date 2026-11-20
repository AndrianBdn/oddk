package operations

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/util"
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
	version, err := resolveCreateVersion(op.params.Version, op.params.Image, op.deps.Docker.ImagePGMajor)
	if err != nil {
		return err
	}
	op.params.Version = version
	if op.params.Port == 0 {
		op.params.Port = 5432
	}

	// Derive image name
	image := op.params.Image
	if image == "" {
		image = fmt.Sprintf("postgres:%s", op.params.Version)
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

	// initdb is given a throwaway, never the instance's real password: the
	// value handed to the entrypoint is kept in Docker's container config for
	// the container's whole lifetime. The real one is set over SQL below, once
	// the cluster is up. See newInitdbCredential.
	initdbPassword := newInitdbCredential()

	containerID, err := op.deps.Docker.CreateContainer(
		op.params.Name,
		op.params.Version,
		image,
		op.params.Port,
		initdbPassword,
		op.params.CPUCores,
		op.params.RAMMB,
		op.params.ParameterGroup,
		parameterGroup.Parameters,
	)
	if err != nil {
		if delErr := op.deps.Store.Instances.Delete(op.params.Name); delErr != nil {
			log.Printf("Error cleaning up instance after container creation failure: %v", delErr)
		}
		if refusal := classifyRecreateError(err); errors.Is(refusal, operr.ErrInvalid) {
			return refusal
		}
		return fmt.Errorf("create container: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateContainerID(op.params.Name, containerID); err != nil {
		op.cleanupFailedCreate(containerID)
		return fmt.Errorf("record container id: %w", err)
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
	if err := waitForPostgresReady(ctx, op.params.Port, initdbPassword); err != nil {
		op.cleanupFailedCreate(containerID)
		return fmt.Errorf("wait for PostgreSQL readiness: %w", annotateReadyError(op.deps, containerID, err))
	}

	// Swap the throwaway for the password that was stored (encrypted) above.
	// Rolling the whole instance back on failure is deliberate: the alternative
	// is a running cluster whose superuser password ODDK does not know, which
	// no later operation could repair.
	if err := adoptPostgresPassword(ctx, op.params.Port, initdbPassword, password); err != nil {
		op.cleanupFailedCreate(containerID)
		return fmt.Errorf("set the instance password: %w", err)
	}

	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, instances.StatusRunning); err != nil {
		return fmt.Errorf("record running status: %w", err)
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
	if err := op.deps.Docker.RemoveVolume(docker.VolumeName(op.params.Name)); err != nil {
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

// minTagMajor and maxTagMajor bound the majors a tag is believed to name. The
// tag parser reads any leading number ("1.2.0" -> 1, "2026.11" -> 2026), and
// recording a release number as the major picks the wrong data mount.
const (
	minTagMajor = 9
	maxTagMajor = 99
)

var versionShapeRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// validateVersionShape refuses an explicit --version that is not exactly "18"
// or "17.2". The major comparisons (parseMajorVersion) trim, but the stored
// string is parsed again where it picks the data mount
// (docker.PGDataMountTarget, which does not), so " 18" would pass every check
// and then mount the volume where a PG 18 cluster is not. Empty is "not given".
func validateVersionShape(version string) error {
	if version != "" && !versionShapeRe.MatchString(version) {
		return operr.Invalidf("invalid --version %q: expected a PostgreSQL major such as %s (or major.minor)", version, DefaultPostgresVersion)
	}
	return nil
}

// resolveCreateVersion settles the major a new instance is recorded with, and
// checks it against the image. The major decides the data-volume mount
// (docker.PGDataMountTarget: PG <= 17 and 18+ differ), so a wrong one puts the
// cluster in an anonymous volume that the next recreate does not bring back.
//
// With an image, the image says. Its PG_MAJOR (imageMajor; the official images
// and everything built on them declare it, and the CLI has pulled the image
// before create) fills an unset version and refuses a contradicting one. So
// does its tag, which switch and update read: a tag naming another major than
// PG_MAJOR or --version is refused, so every instance stays maintainable. To
// fill an unset version without PG_MAJOR, a tag is believed only for a
// plausible major (minTagMajor..maxTagMajor). An image that names its major
// neither way needs --version: guessing the default is exactly the
// wrong-mount case. DefaultPostgresVersion applies only when neither a version
// nor an image is given.
func resolveCreateVersion(version, image string, imageMajor func(string) (int, bool)) (string, error) {
	if err := validateVersionShape(version); err != nil {
		return "", err
	}
	if image == "" {
		if version == "" {
			return DefaultPostgresVersion, nil
		}
		return version, nil
	}

	declared, haveDeclared := imageMajor(image)
	tagged, haveTagged := docker.DetectPGVersionFromImage(image)
	tagMajor, _ := parseMajorVersion(tagged)
	// Against PG_MAJOR the raw tag counts, whatever it looks like: it is what
	// switch and update read.
	if haveDeclared && haveTagged && tagMajor != declared {
		return "", operr.Invalidf("image %s is PostgreSQL %d (its PG_MAJOR) but its tag suggests PostgreSQL %s; tag it with its major (e.g. :pg%d) before creating from it",
			image, declared, tagged, declared)
	}

	if version == "" {
		switch {
		case haveDeclared:
			return strconv.Itoa(declared), nil
		case haveTagged && tagMajor >= minTagMajor && tagMajor <= maxTagMajor:
			// Alone, a tag is believed only for a plausible major:
			// myco/pg:1.2.0 or acme/pg:2026.11 name a release.
			return tagged, nil
		case haveTagged:
			// --version cannot help: it would contradict the tag, which switch
			// and update read.
			return "", operr.Invalidf("image %s declares no PG_MAJOR and its tag reads as PostgreSQL %s, which is a release number, not a major; tag it with its major (e.g. :pg%s) before creating from it",
				image, tagged, DefaultPostgresVersion)
		}
		return "", operr.Invalidf("cannot tell which PostgreSQL major image %s runs (it declares no PG_MAJOR and its tag names none, or it is not pulled); pass --version", image)
	}

	want, _ := parseMajorVersion(version)
	switch {
	case haveDeclared && want != declared:
		return "", operr.Invalidf("image %s is PostgreSQL %d (its PG_MAJOR) but --version %s was specified", image, declared, version)
	case haveTagged && want != tagMajor:
		// The raw tag, as for PG_MAJOR above: switch and update read it.
		return "", operr.Invalidf("image tag suggests PostgreSQL %s but --version %s was specified", tagged, version)
	}
	return version, nil
}
