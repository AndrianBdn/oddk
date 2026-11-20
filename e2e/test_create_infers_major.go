//go:build oddk_debug

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// testCreateInfersMajorFromImage: `create --image` without --version records
// the major the image declares (PG_MAJOR), even when its tag names none. The
// recorded major picks the data-volume mount (PG <= 17 and 18+ differ), so
// guessing the default for a 17-based image put the cluster in an anonymous
// volume that the next recreate did not bring back. A --version that
// contradicts PG_MAJOR is refused.
func testCreateInfersMajorFromImage(h *TestHarness) error {
	log.Println("=== Testing create infers the major from the image's PG_MAJOR ===")

	instanceName := testPrefix + "-infer-major"
	port := 15458
	// A tag with no major in it: neither "postgres:" nor pgNN nor NN-…
	image := testPrefix + "-nomajor:latest"

	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull 17: %w", err)
	}
	if err := h.retagImage("postgres:17", image); err != nil {
		return err
	}
	defer h.removeImage(image)

	log.Println("Step 1: --version contradicting PG_MAJOR is refused")
	_, err := h.runCLI("create", "--name", instanceName, "--image", image, "--version", "18",
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err == nil {
		_ = h.destroyInstanceCLI(instanceName)
		return fmt.Errorf("expected create to refuse --version 18 for a PG_MAJOR=17 image")
	}
	if !strings.Contains(err.Error(), "PG_MAJOR") {
		return fmt.Errorf("refusal should name PG_MAJOR, got: %v", err)
	}

	log.Println("Step 2: no --version records 17 (not the default)")
	output, err := h.runCLI("create", "--name", instanceName, "--image", image,
		"--port", strconv.Itoa(port), "--cpu", "1", "--ram", "512M")
	if err != nil {
		return fmt.Errorf("create without --version: %w (output: %s)", err, output)
	}
	defer func() { _ = h.destroyInstanceCLI(instanceName) }()
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}
	_, body, err := h.request("GET", "/api/rdbms/"+instanceName, nil)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	var inst struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return fmt.Errorf("unmarshal instance: %w", err)
	}
	if inst.Version != "17" {
		return fmt.Errorf("expected recorded version 17 from PG_MAJOR, got %q", inst.Version)
	}

	log.Println("=== Create infers major Test PASSED ===")
	return nil
}
