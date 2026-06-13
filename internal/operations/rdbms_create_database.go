package operations

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/hypersequent/oddk/internal/operr"
)

type CreateDatabaseParams struct {
	InstanceName string
	DatabaseName string
}

type CreateDatabaseResult struct {
	DatabaseName string `json:"databaseName"`
	Message      string `json:"message"`
}

func CreateDatabase(ctx context.Context, deps *Dependencies, params CreateDatabaseParams) (*CreateDatabaseResult, error) {
	if params.InstanceName == "" {
		return nil, fmt.Errorf("instance name is required")
	}
	if params.DatabaseName == "" {
		return nil, fmt.Errorf("database name is required")
	}

	conn, err := ConnectToRunningInstance(ctx, deps, params.InstanceName)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close(ctx)
	}()

	// Check if database already exists
	var exists bool
	checkQuery := "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)"
	if err := conn.QueryRow(ctx, checkQuery, params.DatabaseName).Scan(&exists); err != nil {
		return nil, fmt.Errorf("failed to check if database exists: %w", err)
	}

	if exists {
		return nil, operr.Conflictf("database %s already exists", params.DatabaseName)
	}

	createQuery := fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{params.DatabaseName}.Sanitize())
	if _, err := conn.Exec(ctx, createQuery); err != nil {
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	return &CreateDatabaseResult{
		DatabaseName: params.DatabaseName,
		Message:      fmt.Sprintf("Database %s created successfully", params.DatabaseName),
	}, nil
}
