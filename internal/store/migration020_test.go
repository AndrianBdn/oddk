package store

import (
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/andrianbdn/oddk/internal/store/instances"
)

// Migration 020 rebuilds rdbms_instances, which is the one table a deployment
// cannot lose: it holds the encrypted postgres password for every instance, and
// nothing else has a copy. These tests exist because "rebuild the table" is the
// riskiest shape of migration there is.

// preMigration020Schema is rdbms_instances exactly as migrations 001+006+008+016
// leave it: no CHECK on status.
func preMigration020Schema(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	db.MustExec(`CREATE TABLE rdbms_instances (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		port INTEGER NOT NULL,
		version TEXT NOT NULL,
		status TEXT NOT NULL,
		container_id TEXT,
		password TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		cpu_cores INTEGER NOT NULL DEFAULT 1,
		ram_mb INTEGER NOT NULL DEFAULT 1024,
		parameter_group TEXT NOT NULL DEFAULT 'default:2025-08-27',
		image TEXT NOT NULL DEFAULT ''
	)`)

	return db
}

type instanceRow struct {
	ID             int    `db:"id"`
	Name           string `db:"name"`
	Port           int    `db:"port"`
	Version        string `db:"version"`
	Status         string `db:"status"`
	ContainerID    string `db:"container_id"`
	Password       string `db:"password"`
	CreatedAt      string `db:"created_at"`
	UpdatedAt      string `db:"updated_at"`
	CPUCores       int    `db:"cpu_cores"`
	RAMMB          int    `db:"ram_mb"`
	ParameterGroup string `db:"parameter_group"`
	Image          string `db:"image"`
}

func seedInstance(t *testing.T, db *sqlx.DB, name, status string, port int) {
	t.Helper()
	db.MustExec(`INSERT INTO rdbms_instances
		(name, port, version, status, container_id, password, created_at, updated_at,
		 cpu_cores, ram_mb, parameter_group, image)
		VALUES (?, ?, '17', ?, 'container-'||?, '3ncr.org/1#secret-'||?, '2026-08-01T00:00:00.000000000Z',
		        '2026-08-02T00:00:00.000000000Z', 4, 8192, 'custom-group', 'postgres:17')`,
		name, port, status, name, name)
}

// Every column of every row must survive the rebuild byte-for-byte. A silently
// short or lossy copy here destroys a deployment's instance registry.
func TestMigration020_PreservesEveryColumn(t *testing.T) {
	db := preMigration020Schema(t)
	seedInstance(t, db, "billing", "running", 5432)
	seedInstance(t, db, "reporting", "stopped", 5433)
	seedInstance(t, db, "archive", "error", 5434)

	var before []instanceRow
	if err := db.Select(&before, `SELECT * FROM rdbms_instances ORDER BY id`); err != nil {
		t.Fatalf("read before: %v", err)
	}

	if err := migration020InstanceStatusCheck(db); err != nil {
		t.Fatalf("migration020: %v", err)
	}

	var after []instanceRow
	if err := db.Select(&after, `SELECT * FROM rdbms_instances ORDER BY id`); err != nil {
		t.Fatalf("read after: %v", err)
	}

	if len(after) != len(before) {
		t.Fatalf("row count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("row %d changed across the rebuild:\n before: %+v\n after:  %+v", i, before[i], after[i])
		}
	}

	// The old table must be gone, not left behind as a confusing duplicate.
	var leftover int
	if err := db.Get(&leftover,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='rdbms_instances_new'`); err != nil {
		t.Fatalf("check leftover: %v", err)
	}
	if leftover != 0 {
		t.Error("rdbms_instances_new survived the migration")
	}
}

// The point of the migration: a status nothing understands can no longer be
// stored.
func TestMigration020_ConstraintRejectsUnknownStatus(t *testing.T) {
	db := preMigration020Schema(t)
	seedInstance(t, db, "billing", "running", 5432)

	if err := migration020InstanceStatusCheck(db); err != nil {
		t.Fatalf("migration020: %v", err)
	}

	if _, err := db.Exec(`UPDATE rdbms_instances SET status = 'teleporting' WHERE name = 'billing'`); err == nil {
		t.Error("the CHECK constraint accepted an unrecognised status")
	}

	// Every status the Go vocabulary declares must be accepted, or the database
	// and the code have drifted — which is exactly what this migration prevents.
	for _, s := range instances.AllStatuses() {
		if _, err := db.Exec(`UPDATE rdbms_instances SET status = ? WHERE name = 'billing'`, string(s)); err != nil {
			t.Errorf("CHECK rejected %q, which AllStatuses() declares legal: %v", s, err)
		}
	}
}

// A deployment carrying a status this build does not know about must still
// start. Refusing to boot over a stale status value would be far worse than
// quarantining that one instance — and 'error' is what reconciliation would
// conclude anyway.
func TestMigration020_NormalizesUnknownStatusInsteadOfFailing(t *testing.T) {
	db := preMigration020Schema(t)
	seedInstance(t, db, "billing", "running", 5432)
	seedInstance(t, db, "legacy", "some-status-from-the-future", 5433)

	if err := migration020InstanceStatusCheck(db); err != nil {
		t.Fatalf("migration must not fail on an unrecognised stored status: %v", err)
	}

	var status string
	if err := db.Get(&status, `SELECT status FROM rdbms_instances WHERE name = 'legacy'`); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if status != string(instances.StatusError) {
		t.Errorf("unrecognised status became %q, want 'error'", status)
	}

	// The healthy row must be untouched.
	if err := db.Get(&status, `SELECT status FROM rdbms_instances WHERE name = 'billing'`); err != nil {
		t.Fatalf("read billing row: %v", err)
	}
	if status != string(instances.StatusRunning) {
		t.Errorf("a valid status was rewritten to %q", status)
	}
}

// The legacy broken* family must survive the migration so startup
// reconciliation can un-latch it from the container's real state on this very
// boot. Rewriting it to 'error' here would strand those instances, because
// 'error' is terminal and never auto-cleared.
func TestMigration020_KeepsLegacyBrokenStatusesForReconcileToClear(t *testing.T) {
	db := preMigration020Schema(t)
	seedInstance(t, db, "latched", "broken-port", 5432)

	if err := migration020InstanceStatusCheck(db); err != nil {
		t.Fatalf("migration020: %v", err)
	}

	var status string
	if err := db.Get(&status, `SELECT status FROM rdbms_instances WHERE name = 'latched'`); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != string(instances.StatusBrokenPort) {
		t.Errorf("legacy status became %q; it must survive so reconcile can derive the truth "+
			"from Docker — 'error' would be terminal and strand the instance", status)
	}
}

// AUTOINCREMENT must survive, or a recreated instance could reuse an id.
func TestMigration020_PreservesAutoincrement(t *testing.T) {
	db := preMigration020Schema(t)
	seedInstance(t, db, "billing", "running", 5432)

	if err := migration020InstanceStatusCheck(db); err != nil {
		t.Fatalf("migration020: %v", err)
	}

	var ddl string
	if err := db.Get(&ddl,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='rdbms_instances'`); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if !strings.Contains(ddl, "AUTOINCREMENT") {
		t.Errorf("rebuilt table lost AUTOINCREMENT:\n%s", ddl)
	}
	if !strings.Contains(ddl, "name TEXT UNIQUE NOT NULL") {
		t.Errorf("rebuilt table lost the UNIQUE constraint on name:\n%s", ddl)
	}
}
