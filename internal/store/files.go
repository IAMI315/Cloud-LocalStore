package store

import (
	"context"
	"fmt"
)

type FileRecord struct {
	FileID, NormalizedPath, FileType, PlatformID string
	Size, MTimeNS                                int64
}

// ReplaceFileIndex persists one completed scan atomically. Paths absent from
// the scan remain in the index as tombstones for later sync/version handling.
func (s *ClientStore) ReplaceFileIndex(ctx context.Context, records []FileRecord) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file index transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// A completed scan is the current filesystem snapshot. Preserve rows for
	// historical/version bookkeeping, but mark paths absent from this scan as
	// tombstones before upserting the paths that are still present.
	if _, err := tx.ExecContext(ctx, `UPDATE files SET tombstone = 1, updated_at = CURRENT_TIMESTAMP`); err != nil {
		return fmt.Errorf("mark missing files: %w", err)
	}
	for _, record := range records {
		// Preserve the row identity when the platform reports the same file at a
		// different path. This lets later sync stages represent a rename instead
		// of a delete followed by an unrelated create.
		if record.PlatformID != "" {
			if _, err := tx.ExecContext(ctx, `
UPDATE files SET normalized_path = ?, tombstone = 0, updated_at = CURRENT_TIMESTAMP
WHERE platform_id = ? AND normalized_path <> ?`, record.NormalizedPath, record.PlatformID, record.NormalizedPath); err != nil {
				return fmt.Errorf("detect rename %q: %w", record.NormalizedPath, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO files(file_id, normalized_path, file_type, size, mtime_ns, platform_id, tombstone)
VALUES (?, ?, ?, ?, ?, ?, 0)
ON CONFLICT(normalized_path) DO UPDATE SET
 file_id = excluded.file_id,
 file_type = excluded.file_type,
 size = excluded.size,
 mtime_ns = excluded.mtime_ns,
 platform_id = excluded.platform_id,
 tombstone = 0,
 updated_at = CURRENT_TIMESTAMP`, record.FileID, record.NormalizedPath, record.FileType, record.Size, record.MTimeNS, record.PlatformID); err != nil {
			return fmt.Errorf("upsert file %q: %w", record.NormalizedPath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit file index: %w", err)
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
