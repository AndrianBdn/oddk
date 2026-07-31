package main

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// snapshotManifest mirrors operations.SnapshotManifest for assertions.
type snapshotManifest struct {
	FormatVersion int      `json:"formatVersion"`
	OddkVersion   string   `json:"oddkVersion"`
	SourceHost    string   `json:"sourceHost"`
	Migrations    []string `json:"migrations"`
	Instances     []struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		HasData    bool   `json:"hasData"`
		SkipReason string `json:"skipReason"`
	} `json:"instances"`
}

// tarEntryNames returns the archive member names in the order they physically
// appear in the tar stream.
func tarEntryNames(archivePath string) ([]string, error) {
	f, err := os.Open(archivePath) // #nosec G304 - test-controlled path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	var names []string
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		names = append(names, hdr.Name)
	}
	return names, nil
}

// readTarMember returns the contents of one archive member.
func readTarMember(archivePath, member string) ([]byte, error) {
	f, err := os.Open(archivePath) // #nosec G304 - test-controlled path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("member %s not found", member)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == member {
			return io.ReadAll(tr) // #nosec G110 - test-controlled archive
		}
	}
}

func testSnapshotMake(h *TestHarness) error {
	log.Println("=== Testing Snapshot Make ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	stamp := time.Now().Unix()
	runningName := fmt.Sprintf("oddk-danger-funct-snaprun-%d", stamp)
	stoppedName := fmt.Sprintf("oddk-danger-funct-snapstop-%d", stamp)

	log.Println("Step 1: Creating a running instance with a database and user")
	output, err := h.runCLI("create",
		"--name", runningName, "--version", "17",
		"--port", strconv.Itoa(15471), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create running instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(15471); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if output, err = h.createDatabaseCLI(runningName, "snapdb"); err != nil {
		return fmt.Errorf("create database: %w (output: %s)", err, output)
	}
	if output, err = h.addDatabaseUserCLI(runningName, "snapuser", "snapdb", false); err != nil {
		return fmt.Errorf("add db user: %w (output: %s)", err, output)
	}

	log.Println("Step 2: Creating a second instance and stopping it (configuration-only path)")
	output, err = h.runCLI("create",
		"--name", stoppedName, "--version", "17",
		"--port", strconv.Itoa(15472), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create second instance: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(15472); err != nil {
		return fmt.Errorf("second instance not ready: %w", err)
	}
	if output, err = h.runCLI("instance", "stop", stoppedName); err != nil {
		return fmt.Errorf("stop instance: %w (output: %s)", err, output)
	}

	log.Println("Step 3: Taking snapshot")
	output, err = h.runCLI("snapshot", "make")
	if err != nil {
		return fmt.Errorf("snapshot make failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "NOT encrypted by the master key") {
		return fmt.Errorf("snapshot output is missing the not-encrypted warning; got: %s", output)
	}
	if !strings.Contains(output, "configuration-only") {
		return fmt.Errorf("snapshot output does not report configuration-only instances; got: %s", output)
	}

	log.Println("Step 4: Locating the snapshot archive")
	backupDir := filepath.Join(h.dataDir, "backups")
	matches, err := filepath.Glob(filepath.Join(backupDir, "snapshot-*.tar.zst"))
	if err != nil || len(matches) != 1 {
		return fmt.Errorf("expected exactly 1 snapshot archive in %s, found %v (err %v)", backupDir, matches, err)
	}
	archivePath := matches[0]

	log.Println("Step 5: Verifying archive layout and member order")
	names, err := tarEntryNames(archivePath)
	if err != nil {
		return fmt.Errorf("read tar entries: %w", err)
	}
	if len(names) == 0 || names[0] != "manifest.json" {
		return fmt.Errorf("manifest.json must be the FIRST archive member so apply can fail fast; order begins: %v",
			names[:min(5, len(names))])
	}

	want := []string{
		"oddk.db",
		filepath.Join("instances", runningName, "globals.sql"),
		filepath.Join("instances", runningName, "databases.json"),
		filepath.Join("instances", runningName, "instance.json"),
		filepath.Join("instances", stoppedName, "instance.json"),
	}
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = true
	}
	for _, w := range want {
		if !present[w] {
			return fmt.Errorf("archive is missing %s; members: %v", w, names)
		}
	}

	// The stopped instance must NOT have carried any database dump.
	for _, n := range names {
		if strings.HasPrefix(n, filepath.Join("instances", stoppedName, "databases")) {
			return fmt.Errorf("stopped instance %s unexpectedly has database content: %s", stoppedName, n)
		}
	}
	// The running instance must have dumped its database.
	foundRunningDB := false
	for _, n := range names {
		if strings.HasPrefix(n, filepath.Join("instances", runningName, "databases", "snapdb")) {
			foundRunningDB = true
			break
		}
	}
	if !foundRunningDB {
		return fmt.Errorf("running instance %s has no dump of snapdb; members: %v", runningName, names)
	}

	log.Println("Step 6: Verifying manifest contents")
	raw, err := readTarMember(archivePath, "manifest.json")
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.FormatVersion != 1 {
		return fmt.Errorf("manifest formatVersion = %d, want 1", manifest.FormatVersion)
	}
	if manifest.OddkVersion == "" || manifest.SourceHost == "" {
		return fmt.Errorf("manifest missing version/host: %+v", manifest)
	}
	if len(manifest.Migrations) == 0 {
		return fmt.Errorf("manifest records no migrations")
	}
	if len(manifest.Instances) != 2 {
		return fmt.Errorf("manifest lists %d instances, want 2", len(manifest.Instances))
	}
	for _, inst := range manifest.Instances {
		switch inst.Name {
		case runningName:
			if !inst.HasData {
				return fmt.Errorf("running instance %s marked hasData=false", inst.Name)
			}
		case stoppedName:
			if inst.HasData {
				return fmt.Errorf("stopped instance %s marked hasData=true", inst.Name)
			}
			if inst.SkipReason == "" {
				return fmt.Errorf("stopped instance %s has no skipReason", inst.Name)
			}
		default:
			return fmt.Errorf("unexpected instance in manifest: %s", inst.Name)
		}
	}

	log.Println("Step 7: Verifying the embedded oddk.db is a usable SQLite database")
	dbBytes, err := readTarMember(archivePath, "oddk.db")
	if err != nil {
		return fmt.Errorf("read oddk.db: %w", err)
	}
	if len(dbBytes) < 16 || string(dbBytes[:15]) != "SQLite format 3" {
		return fmt.Errorf("embedded oddk.db is not a SQLite database (len %d)", len(dbBytes))
	}

	log.Println("Step 8: Confirming no staging directory was left behind")
	leftovers, _ := filepath.Glob(filepath.Join(backupDir, ".snapshot-*"))
	if len(leftovers) != 0 {
		return fmt.Errorf("snapshot staging directories left behind: %v", leftovers)
	}

	log.Println("Step 9: Cleaning up")
	if output, err = h.runCLI("instance", "destroy", runningName, "--force"); err != nil {
		return fmt.Errorf("destroy running instance: %w (output: %s)", err, output)
	}
	if output, err = h.runCLI("instance", "destroy", stoppedName, "--force"); err != nil {
		return fmt.Errorf("destroy stopped instance: %w (output: %s)", err, output)
	}

	log.Println("=== Snapshot Make Test PASSED ===")
	return nil
}
