package operations

import (
	"context"
	"fmt"
	"time"
)

type ListDatabasesParams struct {
	InstanceName string
}

type DatabaseInfo struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	Encoding  string    `json:"encoding"`
	Size      string    `json:"size"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

type ListDatabasesResult struct {
	Databases []DatabaseInfo `json:"databases"`
}

func ListDatabases(ctx context.Context, deps *Dependencies, params ListDatabasesParams) (*ListDatabasesResult, error) {
	if params.InstanceName == "" {
		return nil, fmt.Errorf("instance name is required")
	}

	conn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close(ctx)
	}()

	query := `
		SELECT 
			d.datname AS name,
			pg_catalog.pg_get_userbyid(d.datdba) AS owner,
			pg_catalog.pg_encoding_to_char(d.encoding) AS encoding,
			pg_catalog.pg_size_pretty(pg_catalog.pg_database_size(d.datname)) AS size
		FROM pg_catalog.pg_database d
		WHERE d.datistemplate = false
		ORDER BY d.datname
	`

	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query databases: %w", err)
	}
	defer rows.Close()

	var databases []DatabaseInfo
	for rows.Next() {
		var db DatabaseInfo
		if err := rows.Scan(&db.Name, &db.Owner, &db.Encoding, &db.Size); err != nil {
			return nil, fmt.Errorf("failed to scan database row: %w", err)
		}
		databases = append(databases, db)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating database rows: %w", err)
	}

	return &ListDatabasesResult{
		Databases: databases,
	}, nil
}
