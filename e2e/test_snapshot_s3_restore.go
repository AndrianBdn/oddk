package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
)

// setHermeticAWSEnv pins the AWS default credential chain for the test
// PROCESS (CLI and daemon share it): the given env credentials resolve, the
// developer's real ~/.aws files and IMDS do not. Returns a restore function —
// leaked AWS_* variables would redirect every later offsite test.
func setHermeticAWSEnv(extra map[string]string) func() {
	vars := map[string]string{
		"AWS_SHARED_CREDENTIALS_FILE": "/nonexistent-oddk-e2e",
		"AWS_CONFIG_FILE":             "/nonexistent-oddk-e2e",
		"AWS_EC2_METADATA_DISABLED":   "true",
		"AWS_ACCESS_KEY_ID":           "test-key",
		"AWS_SECRET_ACCESS_KEY":       "test-secret",
	}
	maps.Copy(vars, extra)

	saved := map[string]*string{}
	for k, v := range vars {
		if old, ok := os.LookupEnv(k); ok {
			oldCopy := old
			saved[k] = &oldCopy
		} else {
			saved[k] = nil
		}
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, old := range saved {
			if old == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *old)
			}
		}
	}
}

// applyOffsiteFakeS3 points the daemon's offsite settings at the harness's
// fake S3 server.
func applyOffsiteFakeS3(h *TestHarness, bucket, bucketPath string) error {
	config := map[string]any{
		"type":            "s3",
		"bucket":          bucket,
		"endpoint":        h.fakeS3URL,
		"accessKeyId":     "test-key",
		"secretAccessKey": "test-secret",
		"bucketPath":      bucketPath,
		"region":          "us-east-1",
	}
	configFile := filepath.Join(h.dataDir, "offsite-config.json")
	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(configFile, configData, 0o600); err != nil {
		return err
	}
	if output, err := h.runCLI("offsite", "apply", "--file", configFile); err != nil {
		return fmt.Errorf("apply offsite config: %w (output: %s)", err, output)
	}
	return nil
}

// testSnapshotRestoreInstanceFromS3 covers the batteries-included restore of
// this deployment's OWN snapshot: `restore-instance --id` when the local copy
// is gone must download the archive via the offsite settings, make it the
// catalogue row's managed local copy, and then restore from it. Plus both
// modes of `snapshot list-remote` — the command that finds the id/URI in the
// first place.
func testSnapshotRestoreInstanceFromS3(h *TestHarness) error {
	log.Println("=== Testing Restore-Instance From S3 (--id auto-download + list-remote) ===")

	restoreEnv := setHermeticAWSEnv(nil)
	defer restoreEnv()

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		port   = 15482
		dbName = "s3rdb"
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-s3r-%d", time.Now().Unix())

	log.Println("Step 1: A deployment with data, offsite configured")
	output, err := h.runCLI("create",
		"--name", instanceName, "--version", "17",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if output, err = h.createDatabaseCLI(instanceName, dbName); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	pgPassword, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("read postgres password: %w", err)
	}
	pgPassword = strings.TrimSpace(pgPassword)
	if err := h.execSQLAsUser(port, dbName, "postgres", pgPassword,
		"CREATE TABLE things (id int primary key); INSERT INTO things VALUES (1),(2),(3);"); err != nil {
		return fmt.Errorf("seed data: %w", err)
	}
	if err := applyOffsiteFakeS3(h, "test-s3-restore", "oddk/"); err != nil {
		return err
	}

	log.Println("Step 2: Snapshot, upload, and list the bucket both ways")
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	records, err := listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 catalogue record, got %d", len(records))
	}
	id := records[0].ID
	filename := records[0].Filename
	if output, err = h.runCLI("snapshot", "upload", fmt.Sprintf("%d", id)); err != nil {
		return fmt.Errorf("snapshot upload: %w (output: %s)", err, output)
	}

	// Zero-arg list-remote: the daemon lists its own offsite bucket.
	output, err = h.runCLI("snapshot", "list-remote")
	if err != nil {
		return fmt.Errorf("list-remote (daemon mode): %w (output: %s)", err, output)
	}
	if !strings.Contains(output, filename) {
		return fmt.Errorf("daemon-mode list-remote does not show %s: %s", filename, output)
	}
	if !strings.Contains(output, "Rebuild a whole host:") {
		return fmt.Errorf("list-remote is missing the actionable hints: %s", output)
	}

	// Direct-URI list-remote: ambient credentials, no daemon involvement —
	// the exact form a disaster-recovery host uses before apply.
	output, err = h.runCLI("snapshot", "list-remote", "s3://test-s3-restore/oddk", "--endpoint", h.fakeS3URL)
	if err != nil {
		return fmt.Errorf("list-remote (direct mode): %w (output: %s)", err, output)
	}
	if !strings.Contains(output, filename) {
		return fmt.Errorf("direct-mode list-remote does not show %s: %s", filename, output)
	}

	log.Println("Step 3: Drop the local copy, then change the data")
	if output, err = h.runCLI("snapshot", "remove-local", fmt.Sprintf("%d", id), "--force"); err != nil {
		return fmt.Errorf("remove-local: %w (output: %s)", err, output)
	}
	if err := h.execSQLAsUser(port, dbName, "postgres", pgPassword,
		"INSERT INTO things VALUES (4),(5);"); err != nil {
		return fmt.Errorf("mutate data after snapshot: %w", err)
	}

	log.Println("Step 4: restore-instance --id downloads the archive and restores")
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName, "--id", fmt.Sprintf("%d", id), "--yes")
	if err != nil {
		return fmt.Errorf("restore-instance --id: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "downloaded from S3") || !strings.Contains(output, "now the snapshot's local copy") {
		return fmt.Errorf("restore did not report the catalogue download: %s", output)
	}

	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready after restore: %w", err)
	}
	conn, err := pgConnect(port, "postgres", pgPassword, dbName)
	if err != nil {
		return fmt.Errorf("connect after restore: %w", err)
	}
	var rows int
	err = conn.QueryRow(context.Background(), "SELECT count(*) FROM things").Scan(&rows)
	_ = conn.Close(context.Background())
	if err != nil {
		return fmt.Errorf("query restored table: %w", err)
	}
	if rows != 3 {
		return fmt.Errorf("things has %d rows after restore, want the snapshot's 3", rows)
	}

	log.Println("Step 5: The download became the record's managed local copy")
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 1 || records[0].LocalLocation == "" || records[0].RemoteLocation == "" {
		return fmt.Errorf("after the auto-download the record should carry both copies: %+v", records)
	}
	if _, statErr := os.Stat(records[0].LocalLocation); statErr != nil {
		return fmt.Errorf("recorded local copy %q is not on disk: %v", records[0].LocalLocation, statErr)
	}

	// A second --id restore must use the local copy, not download again.
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName, "--id", fmt.Sprintf("%d", id), "--yes")
	if err != nil {
		return fmt.Errorf("second restore-instance --id: %w (output: %s)", err, output)
	}
	if strings.Contains(output, "downloaded from S3") {
		return fmt.Errorf("second restore should reuse the local copy, not download: %s", output)
	}

	log.Println("Step 6: An unknown catalogue id is refused")
	if _, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName, "--id", "99999", "--yes"); err == nil {
		return fmt.Errorf("restoring from a nonexistent catalogue id should fail")
	}

	log.Println("Step 7: Cleaning up")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}

	log.Println("=== Restore-Instance From S3 Test PASSED ===")
	return nil
}

// testSnapshotApplyFromS3 is the batteries-included disaster recovery flow:
// wipe the host to a fresh-install state, then rebuild it with nothing but the
// bucket URI, ambient AWS credentials, and the parked master.key — no aws cli,
// no scp, no manually staged archive.
func testSnapshotApplyFromS3(h *TestHarness) error {
	log.Println("=== Testing Snapshot Apply From S3 (DR without external S3 tooling) ===")

	restoreEnv := setHermeticAWSEnv(nil)
	defer restoreEnv()

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		port   = 15484
		dbName = "s3adb"
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-s3a-%d", time.Now().Unix())

	log.Println("Step 1: A deployment with data, snapshotted and uploaded")
	output, err := h.runCLI("create",
		"--name", instanceName, "--version", "17",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if output, err = h.createDatabaseCLI(instanceName, dbName); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	pgPassword, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("read postgres password: %w", err)
	}
	pgPassword = strings.TrimSpace(pgPassword)
	if err := h.execSQLAsUser(port, dbName, "postgres", pgPassword,
		"CREATE TABLE dr (id int primary key); INSERT INTO dr VALUES (1),(2);"); err != nil {
		return fmt.Errorf("seed data: %w", err)
	}
	if err := applyOffsiteFakeS3(h, "test-s3-apply", "dr-site/"); err != nil {
		return err
	}
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	records, err := listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 catalogue record, got %d", len(records))
	}
	if output, err = h.runCLI("snapshot", "upload", fmt.Sprintf("%d", records[0].ID)); err != nil {
		return fmt.Errorf("snapshot upload: %w (output: %s)", err, output)
	}
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	remoteURI := records[0].RemoteLocation
	archiveName := records[0].Filename
	if !strings.HasPrefix(remoteURI, "s3://") {
		return fmt.Errorf("unexpected remote location %q", remoteURI)
	}

	// Park the key off-host; on a real DR it comes from the operator's store.
	offHost, err := os.MkdirTemp("", "oddk-s3a-offhost-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(offHost) }()
	keyCopy := filepath.Join(offHost, "master.key")
	if err := copyFileForTest(filepath.Join(h.dataDir, "master.key"), keyCopy); err != nil {
		return fmt.Errorf("park master key: %w", err)
	}

	log.Println("Step 2: Wiping the host to a fresh-install state")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}
	if err := h.stopDaemon(); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}
	for _, name := range []string{"oddk.db", "oddk.db-wal", "oddk.db-shm", "master.key"} {
		if err := os.Remove(filepath.Join(h.dataDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("wipe %s: %w", name, err)
		}
	}
	// The local archive is wiped too — the whole point is restoring from S3.
	localArchives, _ := filepath.Glob(filepath.Join(h.dataDir, "backups", "snapshot-*.tar.zst"))
	for _, a := range localArchives {
		if err := os.Remove(a); err != nil {
			return fmt.Errorf("remove local archive %s: %w", a, err)
		}
	}
	if err := h.startDaemon(); err != nil {
		return fmt.Errorf("simulate fresh install: %w", err)
	}
	if err := h.stopDaemon(); err != nil {
		return fmt.Errorf("stop freshly installed daemon: %w", err)
	}
	log.Println("Step 2b: Removing the PostgreSQL image to simulate an empty image cache")
	if _, err := h.docker.ImageRemove(context.Background(), "postgres:17",
		image.RemoveOptions{Force: true, PruneChildren: false}); err != nil {
		return fmt.Errorf("remove image: %w", err)
	}

	log.Println("Step 3: apply --s3-uri downloads with ambient credentials and rebuilds the host")
	output, err = h.runCLI("snapshot", "apply",
		"--s3-uri", remoteURI,
		"--endpoint", h.fakeS3URL,
		"--master-key", keyCopy,
		"--data-dir", h.dataDir,
		"--daemon-port", strconv.Itoa(testPort),
		"--yes")
	if err != nil {
		return fmt.Errorf("snapshot apply --s3-uri: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "Downloaded "+remoteURI) {
		return fmt.Errorf("apply did not report the S3 download: %s", output)
	}
	if !strings.Contains(output, "Snapshot applied") {
		return fmt.Errorf("apply did not report success: %s", output)
	}

	// The archive landed in the managed downloads area, not loose in backupDir.
	downloadedPath := filepath.Join(h.dataDir, "backups", "downloads", archiveName)
	if _, statErr := os.Stat(downloadedPath); statErr != nil {
		return fmt.Errorf("downloaded archive not in the managed area at %q: %v", downloadedPath, statErr)
	}

	log.Println("Step 4: The deployment is back")
	if err := h.startDaemon(); err != nil {
		return fmt.Errorf("start daemon after apply: %w", err)
	}
	statusOut, err := h.getInstanceStatusCLI(instanceName)
	if err != nil {
		return fmt.Errorf("instance status after apply: %w (output: %s)", err, statusOut)
	}
	if !strings.Contains(statusOut, "running") {
		return fmt.Errorf("instance is not running after apply: %s", statusOut)
	}
	conn, err := pgConnect(port, "postgres", pgPassword, dbName)
	if err != nil {
		return fmt.Errorf("connect after apply: %w", err)
	}
	var rows int
	err = conn.QueryRow(context.Background(), "SELECT count(*) FROM dr").Scan(&rows)
	_ = conn.Close(context.Background())
	if err != nil {
		return fmt.Errorf("query restored table: %w", err)
	}
	if rows != 2 {
		return fmt.Errorf("dr has %d rows after apply, want 2", rows)
	}

	log.Println("Step 5: Cleaning up")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}

	log.Println("=== Snapshot Apply From S3 Test PASSED ===")
	return nil
}

// testSnapshotRestoreForeignFromS3 covers restoring ANOTHER deployment's
// snapshot straight from S3: the archive has no catalogue row here, the
// deployment's own master key cannot decrypt it, and the daemon has no offsite
// settings for the bucket — so the CLI's ambient credentials and the parked
// --master-key are what make it work. The archive must land in the managed
// downloads area and stay out of the catalogue, or every checklist coverage
// verdict on this host would be driven by a foreign snapshot.
func testSnapshotRestoreForeignFromS3(h *TestHarness) error {
	log.Println("=== Testing Foreign Restore-Instance From S3 (--s3-uri + --master-key) ===")

	restoreEnv := setHermeticAWSEnv(nil)
	defer restoreEnv()

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		port   = 15486
		dbName = "s3fdb"
	)
	instanceName := fmt.Sprintf("oddk-danger-funct-s3f-%d", time.Now().Unix())

	log.Println("Step 1: The SOURCE deployment: instance with data, snapshot uploaded")
	output, err := h.runCLI("create",
		"--name", instanceName, "--version", "17",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if output, err = h.createDatabaseCLI(instanceName, dbName); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	sourcePgPassword, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("read postgres password: %w", err)
	}
	sourcePgPassword = strings.TrimSpace(sourcePgPassword)
	if err := h.execSQLAsUser(port, dbName, "postgres", sourcePgPassword,
		"CREATE TABLE foreign_rows (id int primary key); INSERT INTO foreign_rows VALUES (1),(2);"); err != nil {
		return fmt.Errorf("seed data: %w", err)
	}
	if err := applyOffsiteFakeS3(h, "test-s3-foreign", "site-a/"); err != nil {
		return err
	}
	if output, err = h.runCLI("snapshot", "make"); err != nil {
		return fmt.Errorf("snapshot make: %w (output: %s)", err, output)
	}
	records, err := listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 1 {
		return fmt.Errorf("expected 1 catalogue record, got %d", len(records))
	}
	if output, err = h.runCLI("snapshot", "upload", fmt.Sprintf("%d", records[0].ID)); err != nil {
		return fmt.Errorf("snapshot upload: %w (output: %s)", err, output)
	}
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	remoteURI := records[0].RemoteLocation
	archiveName := records[0].Filename

	offHost, err := os.MkdirTemp("", "oddk-s3f-offhost-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(offHost) }()
	sourceKey := filepath.Join(offHost, "master.key")
	if err := copyFileForTest(filepath.Join(h.dataDir, "master.key"), sourceKey); err != nil {
		return fmt.Errorf("park source master key: %w", err)
	}

	log.Println("Step 2: Becoming a DIFFERENT deployment (new oddk.db, new master.key)")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}
	if err := h.stopDaemon(); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}
	for _, name := range []string{"oddk.db", "oddk.db-wal", "oddk.db-shm", "master.key"} {
		if err := os.Remove(filepath.Join(h.dataDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("wipe %s: %w", name, err)
		}
	}
	// The source's local archive must go too: this deployment should only be
	// able to reach the snapshot through S3.
	localArchives, _ := filepath.Glob(filepath.Join(h.dataDir, "backups", "snapshot-*.tar.zst"))
	for _, a := range localArchives {
		if err := os.Remove(a); err != nil {
			return fmt.Errorf("remove local archive %s: %w", a, err)
		}
	}
	if err := h.startDaemon(); err != nil {
		return fmt.Errorf("start fresh-identity daemon: %w", err)
	}
	// The old token died with the old oddk.db; mint one for the new identity
	// exactly like the harness setup does.
	token, err := h.server.MintToken()
	if err != nil {
		return fmt.Errorf("mint token on new identity: %w", err)
	}
	cliConfig, err := json.MarshalIndent(map[string]string{
		"daemonUrl": fmt.Sprintf("http://localhost:%d", testPort),
		"authToken": token,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(h.dataDir, ".oddk-cli.json"), cliConfig, 0o600); err != nil {
		return fmt.Errorf("rewrite CLI config: %w", err)
	}
	h.authToken = token

	log.Println("Step 3: Foreign restore — no catalogue row, no offsite settings, wrong live key")
	// Without the source's master key this must refuse (the daemon's own fresh
	// key cannot decrypt the archive's stored credentials).
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName,
		"--s3-uri", remoteURI,
		"--endpoint", h.fakeS3URL,
		"--yes")
	if err == nil {
		return fmt.Errorf("foreign restore without --master-key should fail; output: %s", output)
	}
	if !strings.Contains(err.Error(), "master key") {
		return fmt.Errorf("expected a master-key mismatch refusal, got: %v", err)
	}

	// The refused attempt already fetched the archive, and it must SURVIVE the
	// failure — that is the retry story. Delete it here only so the successful
	// attempt below demonstrably downloads rather than reuses.
	downloadedPath := filepath.Join(h.dataDir, "backups", "downloads", archiveName)
	if _, statErr := os.Stat(downloadedPath); statErr != nil {
		return fmt.Errorf("archive should survive a refused restore for retry: %v", statErr)
	}
	if err := os.Remove(downloadedPath); err != nil {
		return fmt.Errorf("clear cached download: %w", err)
	}

	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName,
		"--s3-uri", remoteURI,
		"--endpoint", h.fakeS3URL,
		"--master-key", sourceKey,
		"--yes")
	if err != nil {
		return fmt.Errorf("foreign restore-instance: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "downloaded from S3") {
		return fmt.Errorf("restore did not report the S3 download: %s", output)
	}
	if !strings.Contains(output, "prunes it after 7 days") {
		return fmt.Errorf("restore did not report the downloads-area lifecycle: %s", output)
	}

	log.Println("Step 4: The instance is back — with the SOURCE's postgres password")
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready after restore: %w", err)
	}
	conn, err := pgConnect(port, "postgres", sourcePgPassword, dbName)
	if err != nil {
		return fmt.Errorf("connect with the source's password after restore: %w", err)
	}
	var rows int
	err = conn.QueryRow(context.Background(), "SELECT count(*) FROM foreign_rows").Scan(&rows)
	_ = conn.Close(context.Background())
	if err != nil {
		return fmt.Errorf("query restored table: %w", err)
	}
	if rows != 2 {
		return fmt.Errorf("foreign_rows has %d rows, want 2", rows)
	}

	log.Println("Step 5: The archive is in downloads/, and NOT in the catalogue")
	if _, statErr := os.Stat(downloadedPath); statErr != nil {
		return fmt.Errorf("archive not in the managed downloads area at %q: %v", downloadedPath, statErr)
	}
	records, err = listSnapshotRecords(h)
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return fmt.Errorf("a foreign restore must not create catalogue rows, got %d", len(records))
	}

	log.Println("Step 6: A daemon restart neither deletes the download nor breaks anything")
	if err := h.restartDaemon(); err != nil {
		return fmt.Errorf("restart daemon: %w", err)
	}
	if _, statErr := os.Stat(downloadedPath); statErr != nil {
		return fmt.Errorf("fresh download did not survive the startup sweep: %v", statErr)
	}

	log.Println("Step 7: Re-restoring from the same URI reuses the downloaded copy")
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName,
		"--s3-uri", remoteURI,
		"--endpoint", h.fakeS3URL,
		"--master-key", sourceKey,
		"--yes")
	if err != nil {
		return fmt.Errorf("second foreign restore: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "reused previously downloaded copy") {
		return fmt.Errorf("second restore should reuse the cached archive: %s", output)
	}

	log.Println("Step 8: Same bucket, different prefix — the offsite rung must honour the absolute key")
	// Configure offsite on the NEW identity pointing at the SAME bucket under a
	// DIFFERENT prefix. The URI (site-a/...) now matches the offsite bucket, so
	// the daemon's stored credentials serve the fetch — and the key must be
	// used verbatim: re-scoping it under this deployment's site-b/ prefix would
	// stat a nonexistent object and refuse a perfectly real archive.
	if err := applyOffsiteFakeS3(h, "test-s3-foreign", "site-b/"); err != nil {
		return err
	}
	output, err = h.runCLI("snapshot", "restore-instance",
		"--instance", instanceName,
		"--s3-uri", remoteURI,
		"--endpoint", h.fakeS3URL,
		"--master-key", sourceKey,
		"--yes")
	if err != nil {
		return fmt.Errorf("same-bucket different-prefix restore: %w (output: %s)", err, output)
	}
	// The cached copy carries matching provenance, so this reuses it — which
	// still proves the point: reuse requires the HeadObject on the ABSOLUTE
	// key to succeed through the offsite-credential client.
	if !strings.Contains(output, "reused previously downloaded copy") {
		return fmt.Errorf("expected the offsite-rung fetch to reuse the cached archive: %s", output)
	}

	log.Println("Step 9: Cleaning up")
	if output, err = h.runCLI("instance", "destroy", instanceName, "--force"); err != nil {
		return fmt.Errorf("destroy instance: %w (output: %s)", err, output)
	}

	log.Println("=== Foreign Restore-Instance From S3 Test PASSED ===")
	return nil
}
