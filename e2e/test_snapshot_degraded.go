//go:build oddk_debug

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// testSnapshotDegradedCapture covers the rule that one instance's capture
// failure must not cost every other instance its DR archive.
//
// Before v0.1.71 this scenario produced NO archive AT ALL: any genuine capture
// failure returned an error from stageAllInstances and the whole run aborted.
// With a persistent trigger — the one used here, a postgres password changed
// outside ODDK, or exhausted WAL senders, or wal_level=minimal — and a nightly
// schedule that dedups on the slot start, that meant weeks with no snapshot for
// a deployment that was otherwise perfectly capturable.
//
// What must hold now, and is asserted below: the archive exists and holds the
// healthy instance's data; the broken instance is in the manifest as
// hasData:false with a skipReason that says the capture FAILED; no partial
// capture output is left inside the archive for it; the command still reports
// failure (non-zero exit + a named instance); and `oddk checklist` reports the
// broken instance as config-only rather than covered.
func testSnapshotDegradedCapture(h *TestHarness) error {
	log.Println("=== Testing Degraded Snapshot Capture (one instance fails, the rest are still captured) ===")

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull image failed: %w", err)
	}

	const (
		healthyPort = 15503
		brokenPort  = 15504
		dbName      = "degradedb"
	)
	stamp := time.Now().Unix()
	healthy := fmt.Sprintf("oddk-danger-funct-degr-ok-%d", stamp)
	broken := fmt.Sprintf("oddk-danger-funct-degr-bad-%d", stamp)

	log.Println("Step 1: Creating two instances with data")
	for _, inst := range []struct {
		name string
		port int
	}{{healthy, healthyPort}, {broken, brokenPort}} {
		output, err := h.runCLI("create",
			"--name", inst.name, "--version", "17",
			"--port", strconv.Itoa(inst.port), "--cpu", "1", "--ram", "512M")
		if err != nil {
			return fmt.Errorf("create %s: %w (output: %s)", inst.name, err, output)
		}
		if err := h.waitForPostgreSQL(inst.port); err != nil {
			return fmt.Errorf("PostgreSQL not ready on %s: %w", inst.name, err)
		}
		if output, err = h.createDatabaseCLI(inst.name, dbName); err != nil {
			return fmt.Errorf("create database on %s: %w (output: %s)", inst.name, err, output)
		}
		password, err := h.getPasswordCLI(inst.name, "--plain")
		if err != nil {
			return fmt.Errorf("read password of %s: %w", inst.name, err)
		}
		if err := h.execSQLAsUser(inst.port, dbName, "postgres", strings.TrimSpace(password),
			"CREATE TABLE rows_here (id int primary key); INSERT INTO rows_here VALUES (1),(2);"); err != nil {
			return fmt.Errorf("seed %s: %w", inst.name, err)
		}
	}

	log.Println("Step 2: Breaking ONE instance's capture by changing its password outside ODDK")
	// Physical capture proves the STORED password still authenticates before it
	// takes a base backup, so this is a realistic, persistent capture failure on
	// an instance that is up and serving — exactly the case that used to take the
	// whole night's archive down with it.
	brokenPassword, err := h.getPasswordCLI(broken, "--plain")
	if err != nil {
		return fmt.Errorf("read password of %s: %w", broken, err)
	}
	if err := h.execSQLAsUser(brokenPort, "postgres", "postgres", strings.TrimSpace(brokenPassword),
		"ALTER USER postgres PASSWORD 'changed-outside-oddk';"); err != nil {
		return fmt.Errorf("change password out of band: %w", err)
	}

	log.Println("Step 3: snapshot make must FAIL loudly and still write an archive")
	output, err := h.runCLI("snapshot", "make")
	if err == nil {
		return fmt.Errorf("snapshot make reported success despite an instance that could not be captured; output: %s", output)
	}
	if !strings.Contains(output, "FAILED to be captured") || !strings.Contains(output, broken) {
		return fmt.Errorf("snapshot make did not name the instance that failed; output: %s", output)
	}
	if strings.Contains(output, healthy+" —") {
		return fmt.Errorf("the healthy instance was reported as failed; output: %s", output)
	}

	backupDir := filepath.Join(h.dataDir, "backups")
	archive, err := newestSnapshotArchive(backupDir)
	if err != nil {
		return fmt.Errorf("no archive was produced by a degraded capture: %w", err)
	}

	log.Println("Step 4: The manifest must record exactly which instance is empty, and why")
	raw, err := readTarMember(archive, "manifest.json")
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	seen := map[string]snapshotManifestInstance{}
	for _, e := range manifest.Instances {
		seen[e.Name] = e
	}
	if e, ok := seen[healthy]; !ok || !e.HasData {
		return fmt.Errorf("the healthy instance is not captured with data: %+v", seen[healthy])
	}
	brokenEntry, ok := seen[broken]
	if !ok {
		return fmt.Errorf("the failed instance is missing from the manifest entirely: %+v", manifest.Instances)
	}
	if brokenEntry.HasData {
		return fmt.Errorf("the failed instance claims data it does not have: %+v", brokenEntry)
	}
	if !strings.Contains(brokenEntry.SkipReason, "FAILED") {
		return fmt.Errorf("skipReason does not distinguish a FAILED capture from an instance that had nothing to capture: %q",
			brokenEntry.SkipReason)
	}

	log.Println("Step 5: No partial capture output may be archived for the failed instance")
	names, err := tarEntryNames(archive)
	if err != nil {
		return fmt.Errorf("read tar entries: %w", err)
	}
	brokenPrefix := filepath.Join("instances", broken) + string(filepath.Separator)
	sawMeta := false
	for _, n := range names {
		if !strings.HasPrefix(n, brokenPrefix) {
			continue
		}
		switch filepath.Base(n) {
		case "instance.json":
			sawMeta = true
		default:
			return fmt.Errorf("a failed capture left %s in the archive; only instance.json belongs to a configuration-only entry", n)
		}
	}
	if !sawMeta {
		return fmt.Errorf("the failed instance has no instance.json; apply could not rebuild its container")
	}

	log.Println("Step 6: The archive is catalogued, and the checklist tells the truth about coverage")
	listOut, err := h.runCLI("snapshot", "list", "--json")
	if err != nil {
		return fmt.Errorf("snapshot list: %w (output: %s)", err, listOut)
	}
	var catalogue []struct {
		Filename          string `json:"filename"`
		Status            string `json:"status"`
		InstancesWithData int    `json:"instancesWithData"`
		ConfigOnly        int    `json:"configOnly"`
	}
	if err := json.Unmarshal([]byte(listOut), &catalogue); err != nil {
		return fmt.Errorf("parse snapshot list JSON: %w (output: %s)", err, listOut)
	}
	if len(catalogue) != 1 || catalogue[0].Filename != filepath.Base(archive) {
		return fmt.Errorf("the degraded archive was not catalogued; catalogue: %+v", catalogue)
	}
	// The catalogue must carry the split, not just the archive: it is what the
	// checklist reads and what retention counts.
	if catalogue[0].InstancesWithData != 1 || catalogue[0].ConfigOnly != 1 {
		return fmt.Errorf("catalogue records %d with data / %d configuration-only, want 1/1: %+v",
			catalogue[0].InstancesWithData, catalogue[0].ConfigOnly, catalogue[0])
	}

	checklistOut, err := h.runCLI("checklist", "--json")
	if err != nil {
		return fmt.Errorf("checklist: %w (output: %s)", err, checklistOut)
	}
	var checklist struct {
		Instances []struct {
			Name             string `json:"name"`
			SnapshotCoverage struct {
				State string `json:"state"`
			} `json:"snapshotCoverage"`
		} `json:"instances"`
	}
	if err := json.Unmarshal([]byte(checklistOut), &checklist); err != nil {
		return fmt.Errorf("parse checklist JSON: %w (output: %s)", err, checklistOut)
	}
	states := map[string]string{}
	for _, inst := range checklist.Instances {
		states[inst.Name] = inst.SnapshotCoverage.State
	}
	if states[healthy] != "covered" {
		return fmt.Errorf("the healthy instance reads %q in the checklist, want covered", states[healthy])
	}
	if states[broken] != "config-only" {
		return fmt.Errorf("the failed instance reads %q in the checklist, want config-only — an archive holding none of its data must never read as protection", states[broken])
	}

	log.Println("Step 7: Cleaning up")
	for _, name := range []string{healthy, broken} {
		if output, err := h.runCLI("instance", "destroy", name, "--force"); err != nil {
			return fmt.Errorf("destroy %s: %w (output: %s)", name, err, output)
		}
	}

	log.Println("=== Degraded Snapshot Capture Test PASSED ===")
	return nil
}
