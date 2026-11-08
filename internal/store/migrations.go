package store

import (
	"context"
	"database/sql"
	"fmt"
)

type Migration struct {
	Version int
	Name    string
	UpSQL   string
}

var migrations = []Migration{{Version: 1, Name: "initial", UpSQL: `
CREATE TABLE IF NOT EXISTS spaces (space_id TEXT PRIMARY KEY, root_path TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS files (file_id TEXT PRIMARY KEY, normalized_path TEXT NOT NULL UNIQUE, file_type TEXT NOT NULL, size INTEGER NOT NULL DEFAULT 0, mtime_ns INTEGER NOT NULL DEFAULT 0, platform_id TEXT NOT NULL DEFAULT '', current_version_id TEXT, tombstone INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE INDEX IF NOT EXISTS idx_files_path ON files(normalized_path);
`}}

func ApplyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	for _, migration := range migrations {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, migration.Version).Scan(&count); err != nil {
			return fmt.Errorf("check migration %d: %w", migration.Version, err)
		}
		if count != 0 {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", migration.Version, err)
		}
		if _, err = tx.ExecContext(ctx, migration.UpSQL); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name) VALUES (?, ?)`, migration.Version, migration.Name)
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", migration.Version, err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", migration.Version, err)
		}
	}
	return nil
}
