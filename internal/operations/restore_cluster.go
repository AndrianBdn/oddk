package operations

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// maxRestoreJobs caps the parallelism passed to pg_restore -j so a high core
// count doesn't overwhelm a freshly-started cluster.
const maxRestoreJobs = 8

// RestoreClusterParams describes a fresh, empty, running PostgreSQL cluster and
// the extracted backup archive to replay into it.
//
// Password is the plaintext postgres superuser password the cluster was created
// with. It must be the same password the archive's globals.sql carries a hash
// of — normally guaranteed because both come from the same oddk.db row. If it
// is not, applying globals rewrites the postgres role to a hash whose plaintext
// is unknown and the cluster becomes unreachable.
type RestoreClusterParams struct {
	InstanceName  string         // names the ephemeral helper containers
	Image         string         // image the helper containers run
	Port          int            // cluster port on the bridge gateway
	Password      string         // postgres superuser password (plaintext)
	CPUCores      int            // caps pg_restore -j
	ExtractedDir  string         // root of the extracted backup archive
	Databases     []DatabaseMeta // per-database metadata from databases.json
	ExpectedRoles []string       // roles globals.sql must create
}

// RestoreClusterFromArchive replays globals and every database from an
// extracted backup archive into a fresh cluster, then verifies that all
// expected roles and databases exist. It returns the number of user databases
// restored.
//
// This is the full-fidelity restore path: unlike RestoreRDBMS (which restores a
// single database across clusters and therefore strips ownership), it recreates
// each database with its original owner, encoding and collation and restores
// WITH ownership and privileges. It assumes the cluster is empty, so the caller
// owns the decision that overwriting is safe.
//
// Callers are responsible for status bookkeeping and for wrapping errors with
// any recovery hint — every failure here leaves a cluster that is partially
// restored at best.
func RestoreClusterFromArchive(ctx context.Context, deps *Dependencies, p RestoreClusterParams) (int, error) {
	// Apply globals (roles + hashed passwords). ON_ERROR_STOP=0 tolerates
	// the pre-existing postgres role / default ACLs.
	if err := restoreGlobals(ctx, deps, p.InstanceName, p.Image, p.Port, p.Password, p.ExtractedDir); err != nil {
		return 0, fmt.Errorf("restore globals: %w", err)
	}

	// Verify globals actually created every expected role. psql runs with
	// ON_ERROR_STOP=0 (it must tolerate initdb collisions like the bootstrap
	// postgres role), so a role that silently failed to restore would otherwise
	// slip through — catch it here before declaring success.
	if err := verifyRolesPresent(ctx, p.Port, p.Password, p.ExpectedRoles); err != nil {
		return 0, fmt.Errorf("globals restore incomplete: %w", err)
	}

	// Recreate + restore each database with ownership preserved.
	jobs := min(max(p.CPUCores, 1), maxRestoreJobs)

	conn, err := connectDirect(ctx, p.Port, p.Password, "postgres")
	if err != nil {
		return 0, fmt.Errorf("connect to target cluster: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	restored := 0
	for _, db := range p.Databases {
		if db.Name == "template0" || db.Name == "template1" {
			continue
		}

		dbDir := filepath.Join(p.ExtractedDir, "databases", db.Name)
		if _, statErr := os.Stat(dbDir); statErr != nil {
			if db.Name == "postgres" {
				continue // empty admin db not in archive — nothing to restore
			}
			return 0, fmt.Errorf("database %s missing from backup archive", db.Name)
		}

		// The fresh cluster already has a "postgres" database; recreate the
		// others from template0 with their original owner + encoding/collation
		// so collation behavior is preserved. Database-level CREATE grants are
		// replayed after the data restore (see below).
		if db.Name != "postgres" {
			createSQL := buildCreateDatabaseSQL(db.Name, db, true)
			if _, err := conn.Exec(ctx, createSQL); err != nil {
				return 0, fmt.Errorf("create database %s (owner %s): %w", db.Name, db.Owner, err)
			}
		}

		if err := restoreDatabaseWithOwner(ctx, deps, p.InstanceName, p.Image, p.Port, p.Password, dbDir, db.Name, jobs); err != nil {
			return 0, fmt.Errorf("restore database %s: %w", db.Name, err)
		}

		if db.Name != "postgres" {
			// restoreDatabaseWithOwner preserves object ownership and privileges,
			// but database-level CREATE grants live in pg_database.datacl and are
			// not carried by a per-database dump at all. Replay the captured
			// grants for roles present on the fresh cluster (globals restored the
			// roles just above), mirroring the backup-restore path — otherwise an
			// app role that could create schemas beforehand silently loses that
			// right. Missing roles are logged and skipped; a genuine grant failure
			// aborts.
			missingRoles, grantErr := restoreDatabaseCreateGrants(ctx, conn, db.Name, db.CreateGrantees)
			if grantErr != nil {
				return 0, fmt.Errorf("restore CREATE grants on database %s: %w", db.Name, grantErr)
			}
			if len(missingRoles) > 0 {
				log.Printf(
					"WARNING: restore cluster: skipped CREATE grants on database %q for roles absent from the target: %s",
					db.Name, strings.Join(missingRoles, ", "),
				)
			}

			restored++
		}
	}

	// Verify the restored cluster has every expected database.
	presentDBs, err := listUserDatabasesDirect(ctx, p.Port, p.Password)
	if err != nil {
		return 0, fmt.Errorf("verify restored cluster: %w", err)
	}
	for _, db := range p.Databases {
		if db.Name == "template0" || db.Name == "template1" {
			continue
		}
		if !presentDBs[db.Name] {
			return 0, fmt.Errorf("verification failed: database %s missing after restore", db.Name)
		}
	}

	return restored, nil
}
