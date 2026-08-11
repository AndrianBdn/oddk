//go:build oddk_debug

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// instanceImageInfo fetches an instance's current image/version/status via the API.
func (h *TestHarness) instanceImageInfo(name string) (image, version, status string, err error) {
	code, body, err := h.request("GET", "/api/rdbms/"+name, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("get instance: %w", err)
	}
	if code != http.StatusOK {
		return "", "", "", fmt.Errorf("get instance: expected 200, got %d: %s", code, body)
	}
	var inst struct {
		Image   string `json:"image"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return "", "", "", fmt.Errorf("unmarshal instance: %w", err)
	}
	return inst.Image, inst.Version, inst.Status, nil
}

// testInstanceUpdate exercises `oddk instance update` against real registry tags:
// the re-pull "up to date" path, the no-op update, and an override update that
// recreates the container while preserving data.
func testInstanceUpdate(h *TestHarness) error {
	log.Println("=== Testing Instance Update ===")

	instanceName := testPrefix + "-update"
	port := 15462

	log.Println("Step 1: Re-pulling postgres:16 reports 'up to date' on the second pull")
	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("first pull postgres:16 failed: %w", err)
	}
	out, err := h.pullImageCLI("16")
	if err != nil {
		return fmt.Errorf("second pull postgres:16 failed: %w", err)
	}
	// Proves pull no longer short-circuits on a present image: it re-checks the
	// registry and Docker reports the tag is current.
	if !strings.Contains(out, "up to date") {
		return fmt.Errorf("second pull should report 'up to date', got: %s", out)
	}

	log.Println("Step 2: Pulling postgres:16-alpine (override target)")
	if _, err := h.pullImageWithImageFlagCLI("postgres:16-alpine"); err != nil {
		return fmt.Errorf("pull postgres:16-alpine failed: %w", err)
	}

	log.Println("Step 3: Creating instance on postgres:16")
	if out, err := h.createInstanceWithImageCLI(instanceName, port, "postgres:16", "16"); err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, out)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	log.Println("Step 4: Creating marker database before update")
	if _, err := h.createDatabaseCLI(instanceName, "updatetest"); err != nil {
		return fmt.Errorf("create marker database failed: %w", err)
	}

	log.Println("Step 5: `instance update` with no change reports up to date")
	out, err = h.updateInstanceCLI(instanceName)
	if err != nil {
		return fmt.Errorf("update (no-op) failed: %w (output: %s)", err, out)
	}
	if !strings.Contains(out, "up to date") {
		return fmt.Errorf("no-op update should report 'up to date', got: %s", out)
	}
	if img, _, _, err := h.instanceImageInfo(instanceName); err != nil {
		return err
	} else if img != "postgres:16" {
		return fmt.Errorf("image should still be postgres:16 after no-op update, got %q", img)
	}

	log.Println("Step 6: `instance update --image postgres:16-alpine` recreates the container")
	out, err = h.updateInstanceWithImageCLI(instanceName, "postgres:16-alpine")
	if err != nil {
		return fmt.Errorf("override update failed: %w (output: %s)", err, out)
	}
	if !strings.Contains(out, "postgres:16-alpine") {
		return fmt.Errorf("override update output should mention the new image, got: %s", out)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready after update: %w", err)
	}

	log.Println("Step 7: Verifying new image and data persistence")
	img, version, status, err := h.instanceImageInfo(instanceName)
	if err != nil {
		return err
	}
	if img != "postgres:16-alpine" {
		return fmt.Errorf("expected image postgres:16-alpine after update, got %q", img)
	}
	if version != "16" {
		return fmt.Errorf("expected version 16 after update, got %q", version)
	}
	if status != "running" {
		return fmt.Errorf("expected status running after update, got %q", status)
	}
	dbList, err := h.listDatabasesCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list databases after update failed: %w", err)
	}
	if !strings.Contains(dbList, "updatetest") {
		return fmt.Errorf("marker database should persist across update: %s", dbList)
	}

	log.Println("Step 8: Cleaning up")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== Instance Update Test PASSED ===")
	return nil
}

// testImageRepullSwitch proves the ID-aware switch fix: when a moving tag is
// re-pulled to a new image ID (a patch release), `instance switch` onto the
// SAME tag now recreates the container to adopt it, instead of refusing with
// "already uses image". A local tag is retagged to a different image to
// simulate the re-pull deterministically (no dependence on registry timing).
func testImageRepullSwitch(h *TestHarness) error {
	log.Println("=== Testing Re-pulled Patch Switch (ID-aware) ===")

	instanceName := testPrefix + "-repull"
	port := 15463
	localTag := testPrefix + "-repull:16" // a moving tag we control

	log.Println("Step 1: Pulling postgres:16 and postgres:16-alpine")
	if _, err := h.pullImageCLI("16"); err != nil {
		return fmt.Errorf("pull postgres:16 failed: %w", err)
	}
	if _, err := h.pullImageWithImageFlagCLI("postgres:16-alpine"); err != nil {
		return fmt.Errorf("pull postgres:16-alpine failed: %w", err)
	}

	log.Println("Step 2: Tagging local moving tag to the 'old patch' (alpine)")
	if err := h.retagImage("postgres:16-alpine", localTag); err != nil {
		return fmt.Errorf("retag (old) failed: %w", err)
	}

	log.Println("Step 3: Creating instance on the moving tag")
	if out, err := h.createInstanceWithImageCLI(instanceName, port, localTag, "16"); err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, out)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if _, err := h.createDatabaseCLI(instanceName, "repulltest"); err != nil {
		return fmt.Errorf("create marker database failed: %w", err)
	}

	log.Println("Step 4: Re-pointing the moving tag to a 'new patch' (different image ID)")
	if err := h.retagImage("postgres:16", localTag); err != nil {
		return fmt.Errorf("retag (new) failed: %w", err)
	}

	log.Println("Step 5: Switching onto the SAME tag now recreates (adopts the new patch)")
	out, err := h.switchInstanceCLI(instanceName, localTag)
	if err != nil {
		return fmt.Errorf("switch onto re-pulled tag should succeed, got: %w (output: %s)", err, out)
	}
	if !strings.Contains(out, "switched successfully") {
		return fmt.Errorf("switch output should indicate success: %s", out)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready after switch: %w", err)
	}

	log.Println("Step 6: Verifying status and data persistence")
	_, _, status, err := h.instanceImageInfo(instanceName)
	if err != nil {
		return err
	}
	if status != "running" {
		return fmt.Errorf("expected status running after switch, got %q", status)
	}
	dbList, err := h.listDatabasesCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list databases after switch failed: %w", err)
	}
	if !strings.Contains(dbList, "repulltest") {
		return fmt.Errorf("marker database should persist across switch: %s", dbList)
	}

	log.Println("Step 7: Switching onto the same (now-current) tag is a no-op and is rejected")
	_, err = h.switchInstanceCLI(instanceName, localTag)
	if err == nil {
		return fmt.Errorf("switch with no change should have failed")
	}
	if !strings.Contains(err.Error(), "up to date") {
		return fmt.Errorf("no-change switch should report up to date, got: %v", err)
	}

	log.Println("Step 8: Cleaning up")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== Re-pulled Patch Switch Test PASSED ===")
	return nil
}
