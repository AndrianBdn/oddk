package operations

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/klauspost/compress/zstd"

	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
)

// Physical snapshot capture and restore.
//
// Capture has two modes. A RUNNING instance is captured with pg_basebackup
// over the replication protocol — no locks, no long transaction, and the
// backup is crash-consistent by construction (WAL for the backup window is
// streamed into pg_wal.tar). A STOPPED instance is captured as a cold file
// copy of its data directory streamed out through the Docker API — a stopped
// cluster's data dir is a valid physical backup on its own.
//
// The pg_basebackup helper CANNOT connect over oddk-bridge: the stock image's
// pg_hba has no `host replication` entry for the bridge network (`all` in the
// database column deliberately excludes replication). It instead JOINS THE
// INSTANCE'S NETWORK NAMESPACE and connects to 127.0.0.1:5432, which initdb's
// default `host replication all 127.0.0.1/32 trust` accepts with zero instance
// configuration. A .pgpass file is still mounted so a hand-hardened pg_hba
// that requires a password on that line keeps working.
//
// Restore never runs tooling inside images at all (so busybox-based custom
// images work): the archived tar streams are rewritten in-process and pushed
// through the Docker copy API into the instance's freshly created — and NOT
// yet started — container, whose volume mount the API resolves. On first
// start the official entrypoint sees PG_VERSION, skips initdb, chowns PGDATA,
// and PostgreSQL performs normal (backup or crash) recovery.

// Fixed file names inside instances/<name>/basebackup/.
const (
	physicalBaseTarZst = "base.tar.zst"
	physicalBaseTar    = "base.tar" // pg_basebackup < 15 cannot compress client-side
	physicalWalTar     = "pg_wal.tar"
)

// helperMountBasebackup is where the staging dir is bind-mounted inside the
// pg_basebackup helper.
const helperMountBasebackup = "/oddk-basebackup"

// stagePhysicalBasebackup captures a running instance's cluster into
// instanceDir/basebackup/ using a netns-joined pg_basebackup helper container.
func stagePhysicalBasebackup(
	ctx context.Context,
	deps *Dependencies,
	instance *instances.RDBMSInstance,
	password, instanceDir string,
	spreadCheckpoint bool,
) error {
	if instance.ContainerID == "" {
		return fmt.Errorf("instance %s is running but has no recorded container", instance.Name)
	}

	// Prove the STORED password still authenticates before capturing. The
	// helper itself connects over 127.0.0.1 where replication is trusted, so
	// pg_basebackup succeeds even after an out-of-band ALTER ROLE — and the
	// archive would then embed an oddk.db credential that cannot authenticate
	// to the restored cluster, the exact failure the master-key preflight
	// exists to prevent. The logical path verified this implicitly (pg_dumpall
	// authenticates with scram over the bridge); physical must do it on
	// purpose.
	if err := TestPostgreSQLConnectivityWithPassword(ctx, instance.Port, password); err != nil {
		return operr.Invalidf(
			"the stored postgres password for instance %s no longer authenticates (changed outside ODDK?): %v. A physical snapshot would embed a credential that cannot be used after restore. Fix it with 'oddk instance set-postgres-password %s' and retry",
			instance.Name, err, instance.Name)
	}

	bbDir := filepath.Join(instanceDir, snapshotBasebackupDir)
	if err := os.MkdirAll(bbDir, 0o750); err != nil {
		return fmt.Errorf("create basebackup staging dir: %w", err)
	}

	image := instance.Image
	if image == "" {
		image = fmt.Sprintf("postgres:%s", instance.Version)
	}

	// The helper connects inside the instance's own network namespace, where
	// PostgreSQL always listens on 5432 regardless of the instance's host port.
	cmd := []string{
		"pg_basebackup",
		"-h", "127.0.0.1",
		"-p", "5432",
		"-U", "postgres",
		"-D", helperMountBasebackup,
		"-Ft",
		"-X", "stream",
		"-w",
	}
	// Client-side zstd keeps compression CPU in the helper, off the server.
	// pg_basebackup only grew it in PG 15; older majors stage a plain tar and
	// the outer snapshot archive compresses it once instead.
	if major, ok := parseMajorVersion(instance.Version); ok && major >= 15 {
		cmd = append(cmd, "--compress=client-zstd")
	}
	// A spread checkpoint paces the initial checkpoint over minutes to avoid
	// an I/O spike — right for a scheduled 3am run, wrong for an operator
	// waiting at a prompt.
	if spreadCheckpoint {
		cmd = append(cmd, "--checkpoint=spread")
	} else {
		cmd = append(cmd, "--checkpoint=fast")
	}

	err := runHelperContainer(ctx, deps, helperContainerSpec{
		ContainerName: fmt.Sprintf("oddk-snapshot-bb-%s-%d", instance.Name, time.Now().UnixNano()),
		Image:         image,
		Cmd:           cmd,
		Password:      password,
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: bbDir,
				Target: helperMountBasebackup,
			},
		},
		JoinNetNSContainer: instance.ContainerID,
		Tool:               "pg_basebackup",
		FailureHint:        basebackupFailureHint,
	})
	if err != nil {
		return err
	}

	if _, ok := physicalBasePath(bbDir); !ok {
		return fmt.Errorf("pg_basebackup reported success but produced no %s/%s in %s",
			physicalBaseTarZst, physicalBaseTar, bbDir)
	}
	// A cluster with tablespaces makes pg_basebackup emit one extra <oid>.tar
	// per tablespace, which restore does not stream (and whose pg_tblspc
	// symlinks it refuses). Refuse at CAPTURE time — an archive that can never
	// be restored must not be catalogued as a completed snapshot.
	if err := refuseUnexpectedBasebackupFiles(bbDir); err != nil {
		return err
	}
	return nil
}

// refuseUnexpectedBasebackupFiles errors when pg_basebackup produced members
// beyond the fixed set restore understands — in practice, per-tablespace tars.
func refuseUnexpectedBasebackupFiles(bbDir string) error {
	entries, err := os.ReadDir(bbDir)
	if err != nil {
		return fmt.Errorf("inspect basebackup staging dir: %w", err)
	}
	var unexpected []string
	for _, e := range entries {
		switch e.Name() {
		case physicalBaseTarZst, physicalBaseTar, physicalWalTar, "backup_manifest":
		default:
			unexpected = append(unexpected, e.Name())
		}
	}
	if len(unexpected) > 0 {
		return operr.Invalidf(
			"pg_basebackup produced unexpected files (%s) — the instance appears to use tablespaces, which physical snapshots do not support; use --logical",
			strings.Join(unexpected, ", "))
	}
	return nil
}

// basebackupFailureHint appends an actionable next step when the failure looks
// like a replication-connection refusal (hardened pg_hba, wal_level=minimal, or
// exhausted wal senders on a customised instance) rather than a plain crash.
func basebackupFailureHint(logs string) string {
	lowered := strings.ToLower(logs)
	for _, marker := range []string{"replication", "wal_level", "wal sender", "max_wal_senders", "pg_hba"} {
		if strings.Contains(lowered, marker) {
			return "\nHint: the instance refuses replication connections from 127.0.0.1 (hardened pg_hba.conf, wal_level below 'replica', or no free wal sender slots). Fix the instance's configuration, or take a portable snapshot instead: oddk snapshot make --logical"
		}
	}
	return ""
}

// stagePhysicalCold captures a STOPPED instance's data directory into
// instanceDir/basebackup/base.tar.zst by streaming it out through the Docker
// copy API (which resolves volume mounts even on a stopped container) and
// recompressing in-process. No tooling inside the image is used.
func stagePhysicalCold(ctx context.Context, deps *Dependencies, instance *instances.RDBMSInstance, instanceDir string) error {
	// Refuse a container that is not genuinely stopped, independently of what
	// the caller checked: a file-by-file copy of a live (or paused) cluster has
	// no backup protocol behind it and restores torn. The remaining
	// check-to-copy window is accepted — ODDK's own operations serialize
	// through the executor, so only an out-of-band `docker start` DURING the
	// copy could race it.
	// GetContainerStatus normalizes every existing, non-live container state to
	// "stopped"; anything else (running/paused/restarting/not found) is unsafe.
	if actual, err := deps.Docker.GetContainerStatus(instance.ContainerID); err != nil {
		return fmt.Errorf("determine container state before cold copy: %w", err)
	} else if actual != "stopped" {
		return fmt.Errorf("container of instance %s is %q, not stopped; a cold copy of a non-stopped cluster would be torn", instance.Name, actual)
	}

	pgdata, err := deps.Docker.ContainerPGData(instance.ContainerID, instance.Version)
	if err != nil {
		return err
	}

	bbDir := filepath.Join(instanceDir, snapshotBasebackupDir)
	if err := os.MkdirAll(bbDir, 0o750); err != nil {
		return fmt.Errorf("create basebackup staging dir: %w", err)
	}

	cli := deps.Docker.GetDockerClient()
	reader, _, err := cli.CopyFromContainer(ctx, instance.ContainerID, pgdata)
	if err != nil {
		return fmt.Errorf("read data directory %s from container: %w", pgdata, err)
	}
	defer func() { _ = reader.Close() }()

	outPath := filepath.Join(bbDir, physicalBaseTarZst)
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 - inside our own staging tree
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}

	err = writeColdCopy(reader, out)
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(outPath)
		return fmt.Errorf("cold copy of %s: %w", pgdata, err)
	}
	return nil
}

// writeColdCopy filters and re-roots the docker-cp tar stream (whose members
// are prefixed with the copied directory's basename) into a zstd-compressed
// tar whose members sit at the data-directory root — the same shape as a
// pg_basebackup base tar, so restore has one layout to deal with.
func writeColdCopy(src io.Reader, dst io.Writer) error {
	zw, err := zstd.NewWriter(dst)
	if err != nil {
		return fmt.Errorf("create zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(src)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read data directory stream: %w", err)
		}
		name, ok, err := stripFirstPathComponent(hdr.Name)
		if err != nil {
			return err
		}
		if !ok {
			continue // the copied directory itself
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			return operr.Invalidf(
				"data directory contains a link (%s); tablespaces and hand-placed symlinks are not supported by physical snapshots — use --logical", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported entry %q (type %v) in data directory stream", hdr.Name, hdr.Typeflag)
		}
		if coldCopyExcluded(name, hdr.Typeflag == tar.TypeDir) {
			continue
		}

		hdr.Name = name
		if hdr.Typeflag == tar.TypeDir && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write tar header %s: %w", name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			// #nosec G110 -- tar.Reader caps each read at the entry's declared
			// size, and moving a multi-gigabyte cluster is this code's job.
			if _, err := io.Copy(tw, tr); err != nil {
				return fmt.Errorf("copy %s: %w", name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("finish tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finish zstd: %w", err)
	}
	return nil
}

// coldCopyExcluded mirrors pg_basebackup's exclusion list: runtime state that
// must not survive into a restored cluster. pg_wal is deliberately KEPT — a
// cold copy has no separate WAL stream, and recovery needs it.
func coldCopyExcluded(name string, isDir bool) bool {
	// Ephemeral top-level files.
	switch name {
	case "postmaster.pid", "postmaster.opts", "backup_label.old", "tablespace_map.old", "current_logfiles.tmp":
		return true
	}
	// Relcache init files appear at several depths.
	if !isDir && path.Base(name) == "pg_internal.init" {
		return true
	}
	// Directories whose contents are runtime-only (the directories themselves
	// are kept — PostgreSQL expects them to exist).
	for _, dir := range []string{"pg_dynshmem", "pg_notify", "pg_replslot", "pg_serial", "pg_snapshots", "pg_stat_tmp", "pg_subtrans"} {
		if strings.HasPrefix(name, dir+"/") {
			return true
		}
	}
	return false
}

// stripFirstPathComponent removes the leading path element docker-cp prefixes
// members with ("docker/PG_VERSION" -> "PG_VERSION"). ok is false for the
// bare root entry. Any escaping or absolute name is rejected.
func stripFirstPathComponent(name string) (stripped string, ok bool, err error) {
	clean, err := safeTarMemberName(name)
	if err != nil {
		return "", false, err
	}
	if clean == "" {
		return "", false, nil
	}
	_, rest, found := strings.Cut(clean, "/")
	if !found || rest == "" {
		return "", false, nil // the root directory entry itself
	}
	return rest, true, nil
}

// safeTarMemberName normalises a tar member name and rejects anything that
// would escape the extraction root. Returns "" for the "." entry.
func safeTarMemberName(name string) (string, error) {
	trimmed := strings.TrimPrefix(name, "./")
	clean := path.Clean(trimmed)
	if clean == "." {
		return "", nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe tar member name %q", name)
	}
	return clean, nil
}

// physicalBasePath returns the base tar inside a basebackup staging/extracted
// dir, preferring the compressed form.
func physicalBasePath(bbDir string) (string, bool) {
	for _, candidate := range []string{physicalBaseTarZst, physicalBaseTar} {
		p := filepath.Join(bbDir, candidate)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// verifyPhysicalStaging confirms an extracted physical entry actually carries
// its base tar. Both restore paths call it BEFORE anything destructive, so a
// truncated archive is a refusal rather than a torn-down instance.
func verifyPhysicalStaging(instanceDir, instanceName string) error {
	bbDir := filepath.Join(instanceDir, snapshotBasebackupDir)
	if _, ok := physicalBasePath(bbDir); !ok {
		return operr.Invalidf("snapshot lists instance %s as a physical capture but the archive has no %s/%s for it",
			instanceName, snapshotBasebackupDir, physicalBaseTarZst)
	}
	return nil
}

// restorePhysicalIntoCreatedContainer streams a physical entry's cluster into
// an instance container that has been CREATED BUT NEVER STARTED. The copy API
// resolves the container's volume mount, so the bytes land in the fresh data
// volume; on first start the entrypoint sees PG_VERSION, skips initdb, chowns
// PGDATA, and PostgreSQL recovers to consistency.
func restorePhysicalIntoCreatedContainer(ctx context.Context, deps *Dependencies, containerID, version, instanceDir string) error {
	bbDir := filepath.Join(instanceDir, snapshotBasebackupDir)
	basePath, ok := physicalBasePath(bbDir)
	if !ok {
		return operr.Invalidf("archive has no %s/%s for this instance", snapshotBasebackupDir, physicalBaseTarZst)
	}

	mountTarget := docker.PGDataMountTarget(version)
	pgdata, err := deps.Docker.ContainerPGData(containerID, version)
	if err != nil {
		return err
	}
	prefix, err := pgDataPrefix(mountTarget, pgdata)
	if err != nil {
		return err
	}

	// The base tar's members sit at the data-directory root; for PG 18+ they
	// must land under <mount>/<major>/docker, so the stream is re-rooted with
	// the prefix (plus synthetic parent dirs, root-owned 0755 — the entrypoint
	// chowns PGDATA itself but not its parents, which only need to be
	// traversable). The cluster's own PG_VERSION file is captured as it flows
	// past, so a data directory that does not match the recorded major — a
	// moving image tag that drifted majors, or a tampered archive — is refused
	// HERE, before the container ever starts, instead of surfacing as an
	// undiagnosed readiness timeout over a crash-looping postgres.
	var pgVersion bytes.Buffer
	if err := streamTarFileIntoContainer(ctx, deps, containerID, mountTarget, prefix, basePath, true, &pgVersion); err != nil {
		return fmt.Errorf("restore base backup: %w", err)
	}
	archiveMajor, ok := pgMajorFromPGVersion(pgVersion.String())
	if !ok {
		return operr.Invalidf("archive's base backup carries no readable PG_VERSION file; it does not look like a PostgreSQL data directory")
	}
	if recordedMajor, ok := parseMajorVersion(version); ok && archiveMajor != recordedMajor {
		return operr.Invalidf(
			"archive holds a PostgreSQL %d data directory but the instance is recorded as PostgreSQL %s; a %d server cannot start on it. The snapshot's instance.json and its data disagree — restore with a matching image, or use a --logical snapshot",
			archiveMajor, version, recordedMajor)
	}

	// A live capture carries the backup window's WAL separately; a cold copy
	// has it inside the base tar already.
	walPath := filepath.Join(bbDir, physicalWalTar)
	if _, err := os.Stat(walPath); err == nil {
		walPrefix := path.Join(prefix, "pg_wal")
		if err := streamTarFileIntoContainer(ctx, deps, containerID, mountTarget, walPrefix, walPath, false, nil); err != nil {
			return fmt.Errorf("restore streamed WAL: %w", err)
		}
	}
	return nil
}

// pgMajorFromPGVersion parses a cluster's PG_VERSION file content ("17\n", or
// "9.6" for ancient clusters) into its major version.
func pgMajorFromPGVersion(content string) (int, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return 0, false
	}
	major, err := strconv.Atoi(strings.Split(trimmed, ".")[0])
	if err != nil || major <= 0 {
		return 0, false
	}
	return major, true
}

// pgDataPrefix computes where inside the mounted volume the data directory
// lives: "" when PGDATA is the mount target itself (PG <= 17), a relative path
// like "18/docker" otherwise. A PGDATA outside the volume cannot be restored
// into it and is refused.
func pgDataPrefix(mountTarget, pgdata string) (string, error) {
	target := path.Clean(mountTarget)
	data := path.Clean(pgdata)
	if data == target {
		return "", nil
	}
	if rest, ok := strings.CutPrefix(data, target+"/"); ok && rest != "" && !strings.Contains(rest, "..") {
		return rest, nil
	}
	return "", fmt.Errorf("container PGDATA %q is outside the data volume mount %q; this image's layout is not supported by physical restore", pgdata, mountTarget)
}

// streamTarFileIntoContainer pushes one archived tar (optionally
// zstd-compressed) into destPath inside the container, re-rooting every member
// under prefix. When emitPrefixDirs is set, synthetic directory entries for
// the prefix components are emitted first so extraction never depends on the
// engine creating parents implicitly. A non-nil capturePGVersion receives the
// content of the stream's root-level PG_VERSION member.
func streamTarFileIntoContainer(
	ctx context.Context,
	deps *Dependencies,
	containerID, destPath, prefix, tarPath string,
	emitPrefixDirs bool,
	capturePGVersion *bytes.Buffer,
) error {
	f, err := os.Open(tarPath) // #nosec G304 - inside our own extracted staging tree
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var src io.Reader = f
	if strings.HasSuffix(tarPath, ".zst") {
		zr, err := zstd.NewReader(f)
		if err != nil {
			return fmt.Errorf("open zstd stream: %w", err)
		}
		defer zr.Close()
		src = zr
	}

	pr, pw := io.Pipe()
	produceErr := make(chan error, 1)
	go func() {
		err := rewriteTarStream(tar.NewReader(src), pw, prefix, emitPrefixDirs, capturePGVersion)
		pw.CloseWithError(err)
		produceErr <- err
	}()

	copyErr := deps.Docker.GetDockerClient().CopyToContainer(ctx, containerID, destPath, pr, container.CopyToContainerOptions{})
	_ = pr.Close() // unblock the producer if the copy aborted early
	prodErr := <-produceErr

	if copyErr != nil {
		return fmt.Errorf("copy into container: %w", copyErr)
	}
	return prodErr
}

// rewriteTarStream copies src's members to a new tar stream on w, prefixing
// every member name and enforcing the same entry-type discipline as the rest
// of ODDK's archive handling (regular files and directories only). A non-nil
// capturePGVersion is filled with the root-level PG_VERSION member's content
// as it flows past — the free by-product that lets restore verify the data
// directory's major before the container ever starts.
func rewriteTarStream(tr *tar.Reader, w io.Writer, prefix string, emitPrefixDirs bool, capturePGVersion *bytes.Buffer) error {
	tw := tar.NewWriter(w)

	if emitPrefixDirs && prefix != "" {
		accumulated := ""
		for part := range strings.SplitSeq(prefix, "/") {
			accumulated = path.Join(accumulated, part)
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     accumulated + "/",
				Mode:     0o755,
				ModTime:  time.Now(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write prefix dir %s: %w", accumulated, err)
			}
		}
	}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, err := safeTarMemberName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			return operr.Invalidf("archive contains a link (%s); tablespaces are not supported by physical restore", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported archive entry %q (type %v)", hdr.Name, hdr.Typeflag)
		}

		hdr.Name = path.Join(prefix, name)
		if hdr.Typeflag == tar.TypeDir {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write tar header %s: %w", hdr.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			var src io.Reader = tr
			if capturePGVersion != nil && name == "PG_VERSION" && hdr.Size <= 64 {
				src = io.TeeReader(tr, capturePGVersion)
			}
			if _, err := io.Copy(tw, src); err != nil {
				return fmt.Errorf("copy %s: %w", hdr.Name, err)
			}
		}
	}
	return tw.Close()
}

// checkPhysicalImageMajor refuses a PHYSICAL entry whose image no longer
// serves the PostgreSQL major the snapshot recorded. The recorded tag normally
// pins the major (postgres:17), but a custom moving tag can drift majors
// between capture and a fresh DR pull — and a physical data directory only
// starts on its own major, a failure that would otherwise surface after the
// destructive phase as an undiagnosed readiness timeout. Best-effort: an image
// without PG_MAJOR in its env skips the check (logical entries are exempt by
// design — logical restore is how clusters MOVE between majors).
func checkPhysicalImageMajor(dockerClient *docker.Client, entry SnapshotInstanceEntry) error {
	if !entry.HasData || entryFormat(entry) != SnapshotFormatPhysical || entry.Image == "" {
		return nil
	}
	recordedMajor, ok := parseMajorVersion(entry.Version)
	if !ok {
		return nil
	}
	imageMajor, ok := dockerClient.ImagePGMajor(entry.Image)
	if !ok {
		return nil
	}
	if imageMajor != recordedMajor {
		return operr.Invalidf(
			"image %s now serves PostgreSQL %d but instance %q was captured on PostgreSQL %s; a physical data directory only starts on its own major. Pin the image to the captured major, or restore a --logical snapshot",
			entry.Image, imageMajor, entry.Name, entry.Version)
	}
	return nil
}

// countUserDatabases reports how many non-template databases (excluding the
// built-in postgres database) a freshly restored cluster serves — the physical
// counterpart of the logical restore's per-database count.
func countUserDatabases(ctx context.Context, port int, password string) (int, error) {
	set, err := listUserDatabasesDirect(ctx, port, password)
	if err != nil {
		return 0, err
	}
	count := 0
	for name := range set {
		if name != "postgres" {
			count++
		}
	}
	return count, nil
}
