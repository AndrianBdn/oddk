//go:build oddk_debug

package main

import (
	"fmt"
	"log"
	"strings"
)

// testCreateAutoPull verifies that `create` provisions its image automatically:
// no separate `oddk pull` is needed first. It uses postgres:15-alpine (not
// referenced by any other test) and removes it up front so the auto-pull
// exercises the real download + streamed progress path.
func testCreateAutoPull(h *TestHarness) error {
	log.Println("=== Testing Create Auto-Pull ===")

	instanceName := testPrefix + "-autopull"
	port := 15464
	img := "postgres:15-alpine"

	log.Println("Step 1: Removing the image so create must pull it")
	h.removeImage(img)

	log.Println("Step 2: Creating instance WITHOUT a prior pull")
	out, err := h.createInstanceWithImageCLI(instanceName, port, img, "15")
	if err != nil {
		return fmt.Errorf("create without prior pull failed: %w (output: %s)", err, out)
	}
	if !strings.Contains(out, instanceName) {
		return fmt.Errorf("create output should contain instance name: %s", out)
	}
	// The auto-pull streams progress (or reports the cached image) through the
	// same output, so the image reference must appear.
	if !strings.Contains(out, img) {
		return fmt.Errorf("create output should show the image being provisioned (%s): %s", img, out)
	}

	log.Println("Step 3: Waiting for PostgreSQL and exercising the instance")
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	if _, err := h.createDatabaseCLI(instanceName, "autopulltest"); err != nil {
		return fmt.Errorf("create database failed: %w", err)
	}

	log.Println("Step 4: Cleaning up")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== Create Auto-Pull Test PASSED ===")
	return nil
}
