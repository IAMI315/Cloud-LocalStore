package store

import (
	"context"
	"database/sql"
	"fmt"
)

type FileRecord struct {
	FileID, NormalizedPath, FileType, PlatformID string
	Size, MTimeNS                                int64
}

// ReplaceFileIndex persists one completed scan atomically. Paths absent from
// the scan remain in the index as tombstones for later sync/version handling.
func (s *ClientStore) ReplaceFileIndex(ctx context.Context, records []FileRecord) error {
	return s.ReplaceFileIndexWithOptions(ctx, records, true)
}

// ReplaceFileIndexWithOptions persists one completed scan atomically. When
// markMissing is false, paths absent from the scan are retained as-is. This
// is used by the pre-pull scan so a temporary local absence cannot mask a
// remote file that is about to be restored.
func (s *ClientStore) ReplaceFileIndexWithOptions(ctx context.Context, records []FileRecord, markMissing bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file index transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := enqueueDetectedRenames(ctx, tx, records); err != nil {
		return err
	}
	// A completed scan is the current filesystem snapshot. Preserve rows for
	// historical/version bookkeeping, but mark paths absent from this scan as
	// tombstones before upserting the paths that are still present.
	if markMissing {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET tombstone = 1, updated_at = datetime('now','localtime') WHERE tombstone = 0`); err != nil {
			return fmt.Errorf("mark missing files: %w", err)
		}
	}
	for _, record := range records {
		// Preserve the row identity when the platform reports the same file at a
		// different path. This lets later sync stages represent a rename instead
		// of a delete followed by an unrelated create.
		if record.PlatformID != "" {
			if _, err := tx.ExecContext(ctx, `
UPDATE files SET normalized_path = ?, tombstone = 0, updated_at = datetime('now','localtime')
WHERE platform_id = ? AND normalized_path <> ?`, record.NormalizedPath, record.PlatformID, record.NormalizedPath); err != nil {
				return fmt.Errorf("detect rename %q: %w", record.NormalizedPath, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO files(file_id, normalized_path, file_type, size, mtime_ns, platform_id, tombstone)
VALUES (?, ?, ?, ?, ?, ?, 0)
ON CONFLICT(normalized_path) DO UPDATE SET
 file_type = excluded.file_type,
 size = excluded.size,
 mtime_ns = excluded.mtime_ns,
 platform_id = excluded.platform_id,
 tombstone = 0,
 updated_at = CASE
   WHEN files.file_type <> excluded.file_type
     OR files.size <> excluded.size
     OR files.mtime_ns <> excluded.mtime_ns
     OR files.platform_id <> excluded.platform_id
     OR files.tombstone <> 0
   THEN datetime('now','localtime')
   ELSE files.updated_at
 END`, record.FileID, record.NormalizedPath, record.FileType, record.Size, record.MTimeNS, record.PlatformID); err != nil {
			return fmt.Errorf("upsert file %q: %w", record.NormalizedPath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit file index: %w", err)
	}
	return nil
}

func enqueueDetectedRenames(ctx context.Context, tx *sql.Tx, records []FileRecord) error {
	for _, record := range records {
		if record.PlatformID == "" {
			continue
		}
		rows, err := tx.QueryContext(ctx, `
SELECT f.normalized_path, COALESCE(f.current_version_id,''), COALESCE(v.manifest_id,'')
FROM files f LEFT JOIN file_versions v ON v.version_id=f.current_version_id
WHERE f.platform_id=? AND f.normalized_path<>? AND f.tombstone=0`, record.PlatformID, record.NormalizedPath)
		if err != nil {
			return fmt.Errorf("find renamed source %q: %w", record.NormalizedPath, err)
		}
		var oldPaths []Tombstone
		for rows.Next() {
			var old Tombstone
			if err := rows.Scan(&old.Path, &old.VersionID, &old.ManifestID); err != nil {
				rows.Close()
				return fmt.Errorf("read renamed source: %w", err)
			}
			oldPaths = append(oldPaths, old)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read renamed sources: %w", err)
		}
		rows.Close()
		for _, old := range oldPaths {
			if err := enqueueRenameDelete(ctx, tx, old); err != nil {
				return err
			}
		}
	}
	return nil
}

func enqueueRenameDelete(ctx context.Context, tx *sql.Tx, old Tombstone) error {
	identity := old.VersionID
	if identity == "" {
		identity = old.ManifestID
	}
	deleteID := "rename-delete:" + old.Path + ":" + identity
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pending_rename_deletes(delete_id,normalized_path,version_id,manifest_id) VALUES(?,?,?,?)`, deleteID, old.Path, old.VersionID, old.ManifestID); err != nil {
		return fmt.Errorf("queue renamed path deletion %q: %w", old.Path, err)
	}
	return nil
}

func (s *ClientStore) CountFiles(ctx context.Context) (int, error) {
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM files`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *ClientStore) CountActiveFiles(ctx context.Context) (int, error) {
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM files WHERE tombstone = 0`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
