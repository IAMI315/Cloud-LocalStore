package store

import (
	"context"
	"database/sql"
	"fmt"
)

// OpenServer has a separate migration namespace. Revision, device, object and
// journal tables will be added here in the server stages without coupling the
// server database to the client file index schema.
func OpenServer(ctx context.Context, db *sql.DB) (*ServerStore, error) {
	for _, pragma := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return nil, fmt.Errorf("configure sqlite (%s): %w", pragma, err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS server_schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return nil, fmt.Errorf("create server migration table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspaces (space_id TEXT PRIMARY KEY, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return nil, fmt.Errorf("create workspace table: %w", err)
	}
	return &ServerStore{DB: db}, nil
}

type ServerStore struct{ DB *sql.DB }
