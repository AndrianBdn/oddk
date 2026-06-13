package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// databaseMetadataFile is the name of the per-database metadata file stored at
// the root of a backup archive alongside globals.sql and databases/.
const databaseMetadataFile = "databases.json"

// DatabaseMeta captures the attributes needed to faithfully recreate a database
// (its encoding/collation and owner). It is written into the backup archive at
// backup time and consumed by restore and major-upgrade.
type DatabaseMeta struct {
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	Encoding    string `json:"encoding"`
	Collate     string `json:"collate"`
	Ctype       string `json:"ctype"`
	LocProvider string `json:"locProvider"` // "c" (libc), "i" (icu), "b" (builtin); "c" assumed pre-PG15
}

// captureDatabaseMetadata reads metadata (owner, encoding, collation, locale
// provider) for every non-template database from the live source cluster.
func captureDatabaseMetadata(ctx context.Context, deps *Dependencies, instanceName string, major int) ([]DatabaseMeta, error) {
	conn, err := ConnectToRunningInstance(ctx, deps, instanceName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()

	// datlocprovider exists only from PostgreSQL 15; older clusters are libc.
	providerExpr := "'c'::text"
	if major >= 15 {
		providerExpr = "datlocprovider::text"
	}
	query := fmt.Sprintf(`
		SELECT datname,
		       pg_catalog.pg_get_userbyid(datdba),
		       pg_catalog.pg_encoding_to_char(encoding),
		       datcollate,
		       datctype,
		       %s
		FROM pg_catalog.pg_database
		WHERE datistemplate = false
		ORDER BY datname`, providerExpr)

	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query pg_database: %w", err)
	}
	defer rows.Close()

	var out []DatabaseMeta
	for rows.Next() {
		var m DatabaseMeta
		if err := rows.Scan(&m.Name, &m.Owner, &m.Encoding, &m.Collate, &m.Ctype, &m.LocProvider); err != nil {
			return nil, fmt.Errorf("scan pg_database row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// writeDatabaseMetadata writes the metadata as databases.json into dir (a backup
// staging directory that is about to be archived).
func writeDatabaseMetadata(dir string, metas []DatabaseMeta) error {
	data, err := json.MarshalIndent(metas, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal database metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, databaseMetadataFile), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", databaseMetadataFile, err)
	}
	return nil
}

// readDatabaseMetadata reads databases.json from an extracted backup directory.
// The bool is false (with nil error) when the file is absent — older archives
// predate this metadata, and callers fall back accordingly.
func readDatabaseMetadata(extractedDir string) ([]DatabaseMeta, bool, error) {
	path := filepath.Join(extractedDir, databaseMetadataFile)
	data, err := os.ReadFile(path) // #nosec G304 - path is the daemon's own extracted backup directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", databaseMetadataFile, err)
	}
	var metas []DatabaseMeta
	if err := json.Unmarshal(data, &metas); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", databaseMetadataFile, err)
	}
	return metas, true, nil
}

// buildCreateDatabaseSQL builds a CREATE DATABASE statement that reproduces a
// libc-locale database's encoding and collation under targetName (which may
// differ from m.Name, e.g. restore --restore-as). Owner is set only when
// withOwner is true. Callers must ensure m.LocProvider == "c"; ICU/builtin
// providers are not reproduced here.
func buildCreateDatabaseSQL(targetName string, m DatabaseMeta, withOwner bool) string {
	var b strings.Builder
	b.WriteString("CREATE DATABASE ")
	b.WriteString(pgx.Identifier{targetName}.Sanitize())
	if withOwner {
		b.WriteString(" OWNER = ")
		b.WriteString(pgx.Identifier{m.Owner}.Sanitize())
	}
	b.WriteString(" TEMPLATE template0 ENCODING ")
	b.WriteString(quotePostgresLiteral(m.Encoding))
	b.WriteString(" LC_COLLATE ")
	b.WriteString(quotePostgresLiteral(m.Collate))
	b.WriteString(" LC_CTYPE ")
	b.WriteString(quotePostgresLiteral(m.Ctype))
	return b.String()
}

// parseMajorVersion extracts the leading integer major version from strings
// like "17", "17.2", or "18".
func parseMajorVersion(v string) (int, bool) {
	major := strings.SplitN(strings.TrimSpace(v), ".", 2)[0]
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, false
	}
	return n, true
}
