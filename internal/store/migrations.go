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
CREATE TABLE IF NOT EXISTS spaces (space_id TEXT PRIMARY KEY, root_path TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')));
CREATE TABLE IF NOT EXISTS files (file_id TEXT PRIMARY KEY, normalized_path TEXT NOT NULL UNIQUE, file_type TEXT NOT NULL, size INTEGER NOT NULL DEFAULT 0, mtime_ns INTEGER NOT NULL DEFAULT 0, platform_id TEXT NOT NULL DEFAULT '', current_version_id TEXT, tombstone INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')));
CREATE INDEX IF NOT EXISTS idx_files_path ON files(normalized_path);
`}, {Version: 2, Name: "local_versions", UpSQL: `
CREATE TABLE IF NOT EXISTS chunks (chunk_hash TEXT PRIMARY KEY, size INTEGER NOT NULL, verified_at TEXT NOT NULL DEFAULT (datetime('now','localtime')));
CREATE TABLE IF NOT EXISTS manifests (manifest_id TEXT PRIMARY KEY, size INTEGER NOT NULL, chunk_size INTEGER NOT NULL, chunk_count INTEGER NOT NULL, chunk_hashes TEXT NOT NULL, hash_algorithm TEXT NOT NULL, format_version INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS file_versions (version_id TEXT PRIMARY KEY, file_id TEXT NOT NULL, parent_version_id TEXT, manifest_id TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')), is_conflict INTEGER NOT NULL DEFAULT 0, FOREIGN KEY(file_id) REFERENCES files(file_id));
CREATE INDEX IF NOT EXISTS idx_file_versions_file ON file_versions(file_id, created_at);
`}, {Version: 3, Name: "sync_cursor", UpSQL: `
CREATE TABLE IF NOT EXISTS sync_state (space_id TEXT PRIMARY KEY, last_seen_revision INTEGER NOT NULL DEFAULT 0, last_applied_revision INTEGER NOT NULL DEFAULT 0, last_ack_revision INTEGER NOT NULL DEFAULT 0, device_epoch INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')));
`}, {Version: 4, Name: "sync_outbox", UpSQL: `
CREATE TABLE IF NOT EXISTS sync_outbox (operation_id TEXT PRIMARY KEY, space_id TEXT NOT NULL, normalized_path TEXT NOT NULL, manifest_id TEXT NOT NULL, parent_revision INTEGER NOT NULL, state TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')));
CREATE UNIQUE INDEX IF NOT EXISTS sync_outbox_path_manifest_idx ON sync_outbox(space_id, normalized_path, manifest_id);
`}, {Version: 5, Name: "sync_outbox_operation_type", UpSQL: `
ALTER TABLE sync_outbox ADD COLUMN operation_type TEXT NOT NULL DEFAULT 'update';
DROP INDEX IF EXISTS sync_outbox_path_manifest_idx;
CREATE UNIQUE INDEX IF NOT EXISTS sync_outbox_path_manifest_type_idx ON sync_outbox(space_id, normalized_path, manifest_id, operation_type);
`}, {Version: 6, Name: "pending_rename_deletes", UpSQL: `
CREATE TABLE IF NOT EXISTS pending_rename_deletes (
 delete_id TEXT PRIMARY KEY,
 normalized_path TEXT NOT NULL,
 version_id TEXT NOT NULL DEFAULT '',
 manifest_id TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);
CREATE INDEX IF NOT EXISTS idx_pending_rename_deletes_path ON pending_rename_deletes(normalized_path);
`}, {Version: 7, Name: "sync_outbox_file_version", UpSQL: `
ALTER TABLE sync_outbox ADD COLUMN file_version_id TEXT NOT NULL DEFAULT '';
DROP INDEX IF EXISTS sync_outbox_path_manifest_type_idx;
CREATE UNIQUE INDEX IF NOT EXISTS sync_outbox_path_manifest_type_version_idx ON sync_outbox(space_id, normalized_path, manifest_id, operation_type, file_version_id);
`}, {Version: 8, Name: "local_timezone_timestamps", UpSQL: `
UPDATE spaces SET created_at=datetime(created_at,'utc','localtime') WHERE created_at <> '';
UPDATE files SET updated_at=datetime(updated_at,'utc','localtime') WHERE updated_at <> '';
UPDATE chunks SET verified_at=datetime(verified_at,'utc','localtime') WHERE verified_at <> '';
UPDATE file_versions SET created_at=datetime(created_at,'utc','localtime') WHERE created_at <> '';
UPDATE sync_state SET updated_at=datetime(updated_at,'utc','localtime') WHERE updated_at <> '';
UPDATE sync_outbox SET updated_at=datetime(updated_at,'utc','localtime') WHERE updated_at <> '';
UPDATE pending_rename_deletes SET created_at=datetime(created_at,'utc','localtime') WHERE created_at <> '';
UPDATE schema_migrations SET applied_at=datetime(applied_at,'utc','localtime') WHERE applied_at <> '';
`}}

func ApplyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL DEFAULT (datetime('now','localtime')))`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var existingMigrations int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations`).Scan(&existingMigrations); err != nil {
		return fmt.Errorf("count migrations: %w", err)
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
		// A fresh database already receives local timestamps from migrations 1-7.
		// Only convert legacy UTC rows when the schema existed before this release.
		if migration.Version == 8 && existingMigrations == 0 {
			err = nil
		} else {
			_, err = tx.ExecContext(ctx, migration.UpSQL)
		}
		if err == nil {
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
