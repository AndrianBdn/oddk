package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgConnect opens a direct PostgreSQL connection to a test instance on the
// docker bridge gateway, as the given role.
func pgConnect(port int, user, password, database string) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connStr := fmt.Sprintf("postgres://%s:%s@10.88.0.1:%d/%s?sslmode=disable", user, password, port, database)
	return pgx.Connect(ctx, connStr)
}

func testMajorUpgrade(h *TestHarness) error {
	log.Println("=== Testing Major Version Upgrade (17 -> 18) ===")

	instanceName := testPrefix + "-pgupgrade"
	port := 15451
	ownerPass := "ownerpass123"

	log.Println("Step 1: Pulling PostgreSQL 17 and 18 images")
	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull 17 failed: %w", err)
	}
	if _, err := h.pullImageCLI("18"); err != nil {
		return fmt.Errorf("pull 18 failed: %w", err)
	}

	log.Println("Step 2: Creating PG17 instance")
	output, err := h.runCLI("create",
		"--name", instanceName,
		"--version", "17",
		"--port", strconv.Itoa(port),
		"--cpu", "1",
		"--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	pwOut, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password failed: %w", err)
	}
	password := strings.TrimSpace(pwOut)
	if password == "" {
		return fmt.Errorf("got empty password")
	}

	log.Println("Step 4: Seeding role, database, and owned table")
	ctx := context.Background()
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "postgres")
		if err != nil {
			return fmt.Errorf("connect as postgres: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE ROLE appowner LOGIN PASSWORD '%s'", ownerPass)); err != nil {
			return fmt.Errorf("create role: %w", err)
		}
		if _, err := conn.Exec(ctx, "CREATE DATABASE appdb OWNER appowner"); err != nil {
			return fmt.Errorf("create database: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "appdb")
		if err != nil {
			return fmt.Errorf("connect to appdb: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		if _, err := conn.Exec(ctx, "CREATE TABLE widgets (id int PRIMARY KEY, name text)"); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO widgets (id, name) VALUES (1,'a'),(2,'b'),(3,'c')"); err != nil {
			return fmt.Errorf("insert rows: %w", err)
		}
		if _, err := conn.Exec(ctx, "ALTER TABLE widgets OWNER TO appowner"); err != nil {
			return fmt.Errorf("alter table owner: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}

	log.Println("Step 5: Upgrading to PostgreSQL 18")
	output, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "18", "--yes")
	if err != nil {
		return fmt.Errorf("major-upgrade failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "upgraded successfully") {
		return fmt.Errorf("expected success message, got: %s", output)
	}
	if !strings.Contains(output, "Pre-upgrade backup ID:") {
		return fmt.Errorf("expected pre-upgrade backup ID in output, got: %s", output)
	}

	log.Println("Step 6: Verifying instance metadata after upgrade")
	_, body, err := h.request("GET", "/api/rdbms/"+instanceName, nil)
	if err != nil {
		return fmt.Errorf("get instance failed: %w", err)
	}
	var inst struct {
		Image   string `json:"image"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return fmt.Errorf("unmarshal instance: %w", err)
	}
	if inst.Version != "18" {
		return fmt.Errorf("expected version 18, got %q", inst.Version)
	}
	if inst.Image != "postgres:18" {
		return fmt.Errorf("expected image postgres:18, got %q", inst.Image)
	}
	if inst.Status != "running" {
		return fmt.Errorf("expected status running, got %q", inst.Status)
	}

	log.Println("Step 7: Verifying database survived")
	dbList, err := h.listDatabasesCLI(instanceName)
	if err != nil {
		return fmt.Errorf("list databases failed: %w", err)
	}
	if !strings.Contains(dbList, "appdb") {
		return fmt.Errorf("appdb missing after upgrade: %s", dbList)
	}

	log.Println("Step 8: Verifying data + ownership on the new PG18 cluster")
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "appdb")
		if err != nil {
			return fmt.Errorf("connect to upgraded appdb: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		var verNum int
		if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&verNum); err != nil {
			return fmt.Errorf("query server version: %w", err)
		}
		if verNum/10000 != 18 {
			return fmt.Errorf("expected server major version 18, got server_version_num=%d", verNum)
		}

		var rowCount int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM widgets").Scan(&rowCount); err != nil {
			return fmt.Errorf("count widgets: %w", err)
		}
		if rowCount != 3 {
			return fmt.Errorf("expected 3 rows in widgets, got %d", rowCount)
		}

		var tableOwner string
		if err := conn.QueryRow(ctx, "SELECT tableowner FROM pg_tables WHERE tablename='widgets'").Scan(&tableOwner); err != nil {
			return fmt.Errorf("query table owner: %w", err)
		}
		if tableOwner != "appowner" {
			return fmt.Errorf("expected table owner appowner, got %q", tableOwner)
		}

		var dbOwner string
		if err := conn.QueryRow(ctx, "SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_database WHERE datname='appdb'").Scan(&dbOwner); err != nil {
			return fmt.Errorf("query database owner: %w", err)
		}
		if dbOwner != "appowner" {
			return fmt.Errorf("expected database owner appowner, got %q", dbOwner)
		}
		return nil
	}(); err != nil {
		return err
	}

	log.Println("Step 9: Verifying role login with preserved password")
	if err := func() error {
		conn, err := pgConnect(port, "appowner", ownerPass, "appdb")
		if err != nil {
			return fmt.Errorf("login as appowner failed (password not preserved?): %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		var one int
		if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return fmt.Errorf("appowner query failed: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}

	log.Println("Step 10: Verifying downgrade is rejected")
	_, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "16", "--yes")
	if err == nil {
		return fmt.Errorf("expected downgrade to be rejected")
	}
	if !strings.Contains(err.Error(), "must be greater than current") {
		return fmt.Errorf("expected downgrade rejection message, got: %v", err)
	}

	log.Println("Step 11: Verifying same-version is rejected")
	_, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "18", "--yes")
	if err == nil {
		return fmt.Errorf("expected same-version upgrade to be rejected")
	}
	if !strings.Contains(err.Error(), "must be greater than current") {
		return fmt.Errorf("expected same-version rejection message, got: %v", err)
	}

	log.Println("Step 12: Verifying version/image mismatch is rejected")
	_, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "19", "--image", "postgres:17", "--yes")
	if err == nil {
		return fmt.Errorf("expected version/image mismatch to be rejected")
	}
	if !strings.Contains(err.Error(), "suggests PostgreSQL 17") {
		return fmt.Errorf("expected mismatch rejection message, got: %v", err)
	}

	log.Println("Step 13: Verifying missing target image is rejected")
	_, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "19", "--yes")
	if err == nil {
		return fmt.Errorf("expected missing image to be rejected")
	}
	if !strings.Contains(err.Error(), "not found locally") {
		return fmt.Errorf("expected image-not-found message, got: %v", err)
	}

	log.Println("Step 14: Confirming instance is intact after rejected upgrades")
	_, body, err = h.request("GET", "/api/rdbms/"+instanceName, nil)
	if err != nil {
		return fmt.Errorf("get instance failed: %w", err)
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return fmt.Errorf("unmarshal instance: %w", err)
	}
	if inst.Version != "18" || inst.Status != "running" {
		return fmt.Errorf("instance should be running at version 18, got version=%q status=%q", inst.Version, inst.Status)
	}

	log.Println("Step 15: Cleaning up")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== Major Version Upgrade Test PASSED ===")
	return nil
}

// testMajorUpgradeCustomImage upgrades a custom-image (pgvector) instance across
// a major version, verifying that the extension and its data survive and that
// --image is required for custom images.
func testMajorUpgradeCustomImage(h *TestHarness) error {
	log.Println("=== Testing Major Version Upgrade (custom pgvector image) ===")

	instanceName := testPrefix + "-pgupgrade-vec"
	port := 15452
	srcImage := "pgvector/pgvector:pg17-trixie"
	dstImage := "pgvector/pgvector:pg18-trixie"

	log.Println("Step 1: Pulling pgvector images")
	if _, err := h.pullImageWithImageFlagCLI(srcImage); err != nil {
		return fmt.Errorf("pull %s failed: %w", srcImage, err)
	}
	if _, err := h.pullImageWithImageFlagCLI(dstImage); err != nil {
		return fmt.Errorf("pull %s failed: %w", dstImage, err)
	}

	log.Println("Step 2: Creating PG17 pgvector instance")
	output, err := h.runCLI("create",
		"--name", instanceName,
		"--version", "17",
		"--image", srcImage,
		"--port", strconv.Itoa(port),
		"--cpu", "1",
		"--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	pwOut, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password failed: %w", err)
	}
	password := strings.TrimSpace(pwOut)

	log.Println("Step 3: Seeding pgvector data")
	ctx := context.Background()
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "postgres")
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "CREATE DATABASE vecdb"); err != nil {
			return fmt.Errorf("create vecdb: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "vecdb")
		if err != nil {
			return fmt.Errorf("connect vecdb: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "CREATE EXTENSION vector"); err != nil {
			return fmt.Errorf("create extension: %w", err)
		}
		if _, err := conn.Exec(ctx, "CREATE TABLE items (id int PRIMARY KEY, embedding vector(3))"); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO items (id, embedding) VALUES (1, '[1,2,3]'), (2, '[4,5,6]')"); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}

	log.Println("Step 4: Verifying --image is required for custom images")
	_, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "18", "--yes")
	if err == nil {
		return fmt.Errorf("expected custom-image upgrade without --image to be rejected")
	}
	if !strings.Contains(err.Error(), "custom image") {
		return fmt.Errorf("expected custom-image rejection message, got: %v", err)
	}

	log.Println("Step 5: Upgrading pgvector instance to PG18")
	output, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "18", "--image", dstImage, "--yes")
	if err != nil {
		return fmt.Errorf("major-upgrade failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "upgraded successfully") {
		return fmt.Errorf("expected success message, got: %s", output)
	}

	log.Println("Step 6: Verifying pgvector data after upgrade")
	_, body, err := h.request("GET", "/api/rdbms/"+instanceName, nil)
	if err != nil {
		return fmt.Errorf("get instance failed: %w", err)
	}
	var inst struct {
		Image   string `json:"image"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return fmt.Errorf("unmarshal instance: %w", err)
	}
	if inst.Version != "18" || inst.Image != dstImage || inst.Status != "running" {
		return fmt.Errorf("unexpected instance state: version=%q image=%q status=%q", inst.Version, inst.Image, inst.Status)
	}

	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "vecdb")
		if err != nil {
			return fmt.Errorf("connect upgraded vecdb: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		var hasExt bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='vector')").Scan(&hasExt); err != nil {
			return fmt.Errorf("check extension: %w", err)
		}
		if !hasExt {
			return fmt.Errorf("vector extension missing after upgrade")
		}

		var nearest int
		if err := conn.QueryRow(ctx, "SELECT id FROM items ORDER BY embedding <-> '[1,2,3]' LIMIT 1").Scan(&nearest); err != nil {
			return fmt.Errorf("vector query failed: %w", err)
		}
		if nearest != 1 {
			return fmt.Errorf("expected nearest vector id 1, got %d", nearest)
		}
		return nil
	}(); err != nil {
		return err
	}

	log.Println("Step 7: Cleaning up")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== Major Version Upgrade (custom image) Test PASSED ===")
	return nil
}

// testMajorUpgradeLocale verifies that a non-default (C) locale database keeps
// its collation/ctype across a major upgrade, that a custom role survives (role
// verification path), and that an ICU-locale database is refused up front.
func testMajorUpgradeLocale(h *TestHarness) error {
	log.Println("=== Testing Major Version Upgrade (locale preservation + guard) ===")

	instanceName := testPrefix + "-pgupgrade-locale"
	port := 15453
	ctx := context.Background()

	log.Println("Step 1: Pulling PostgreSQL 17 and 18 images")
	if _, err := h.pullImageCLI("17"); err != nil {
		return fmt.Errorf("pull 17 failed: %w", err)
	}
	if _, err := h.pullImageCLI("18"); err != nil {
		return fmt.Errorf("pull 18 failed: %w", err)
	}

	log.Println("Step 2: Creating PG17 instance")
	output, err := h.runCLI("create",
		"--name", instanceName,
		"--version", "17",
		"--port", strconv.Itoa(port),
		"--cpu", "1",
		"--ram", "512M")
	if err != nil {
		return fmt.Errorf("create instance failed: %w (output: %s)", err, output)
	}
	if err := h.waitForPostgreSQL(port); err != nil {
		return fmt.Errorf("PostgreSQL not ready: %w", err)
	}

	pwOut, err := h.getPasswordCLI(instanceName, "--plain")
	if err != nil {
		return fmt.Errorf("get password failed: %w", err)
	}
	password := strings.TrimSpace(pwOut)

	log.Println("Step 3: Seeding C-locale database and a custom role")
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "postgres")
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "CREATE DATABASE clocaledb TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
			return fmt.Errorf("create clocaledb: %w", err)
		}
		if _, err := conn.Exec(ctx, "CREATE ROLE reader LOGIN PASSWORD 'readerpass'"); err != nil {
			return fmt.Errorf("create role: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "clocaledb")
		if err != nil {
			return fmt.Errorf("connect clocaledb: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "CREATE TABLE t (id int PRIMARY KEY)"); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO t VALUES (1),(2)"); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}

	// Best-effort ICU guard sub-test: if we can create an ICU database, the upgrade must
	// refuse it; then we drop it so the real upgrade proceeds.
	log.Println("Step 4: Verifying ICU-locale database is refused (best-effort)")
	icuCreated := false
	func() {
		conn, err := pgConnect(port, "postgres", password, "postgres")
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "CREATE DATABASE icudb TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER icu ICU_LOCALE 'en-US' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
			log.Printf("(skipping ICU guard sub-test: could not create ICU db: %v)", err)
			return
		}
		icuCreated = true
	}()
	if icuCreated {
		_, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "18", "--yes")
		if err == nil {
			return fmt.Errorf("expected ICU-locale database to block the upgrade")
		}
		if !strings.Contains(err.Error(), "unsupported locale provider") {
			return fmt.Errorf("expected unsupported-locale-provider error, got: %v", err)
		}
		// Drop it so the real upgrade can proceed.
		conn, cerr := pgConnect(port, "postgres", password, "postgres")
		if cerr != nil {
			return fmt.Errorf("connect to drop icudb: %w", cerr)
		}
		if _, derr := conn.Exec(ctx, "DROP DATABASE icudb"); derr != nil {
			_ = conn.Close(ctx)
			return fmt.Errorf("drop icudb: %w", derr)
		}
		_ = conn.Close(ctx)
	}

	log.Println("Step 5: Upgrading to PostgreSQL 18")
	output, err = h.runCLI("instance", "major-upgrade", instanceName, "--target-version", "18", "--yes")
	if err != nil {
		return fmt.Errorf("major-upgrade failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "upgraded successfully") {
		return fmt.Errorf("expected success message, got: %s", output)
	}

	log.Println("Step 6: Verifying C locale, data, and role survived")
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "postgres")
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		var collate, ctype string
		if err := conn.QueryRow(ctx, "SELECT datcollate, datctype FROM pg_database WHERE datname='clocaledb'").Scan(&collate, &ctype); err != nil {
			return fmt.Errorf("query clocaledb locale: %w", err)
		}
		if collate != "C" || ctype != "C" {
			return fmt.Errorf("expected C/C collation, got collate=%q ctype=%q", collate, ctype)
		}

		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='reader')").Scan(&exists); err != nil {
			return fmt.Errorf("query role: %w", err)
		}
		if !exists {
			return fmt.Errorf("role 'reader' missing after upgrade")
		}
		return nil
	}(); err != nil {
		return err
	}
	if err := func() error {
		conn, err := pgConnect(port, "postgres", password, "clocaledb")
		if err != nil {
			return fmt.Errorf("connect clocaledb: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
			return fmt.Errorf("count rows: %w", err)
		}
		if n != 2 {
			return fmt.Errorf("expected 2 rows in t, got %d", n)
		}
		return nil
	}(); err != nil {
		return err
	}

	log.Println("Step 7: Cleaning up")
	if err := h.destroyInstanceCLI(instanceName); err != nil {
		return fmt.Errorf("destroy instance failed: %w", err)
	}

	log.Println("=== Major Version Upgrade (locale) Test PASSED ===")
	return nil
}
