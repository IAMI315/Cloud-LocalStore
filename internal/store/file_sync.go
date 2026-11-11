package store

import (
	"context"
	"fmt"
)

func (s *ClientStore) UpsertFile(ctx context.Context, record FileRecord) error {
	// Keep the existing file_id when a remote operation is applied to a path.
	// Historical file_versions reference that key, so replacing it would
	// violate the foreign key and break synchronization after a restart.
	_, err := s.DB.ExecContext(ctx, `INSERT INTO files(file_id,normalized_path,file_type,size,mtime_ns,platform_id,tombstone) VALUES(?,?,?,?,?,?,0) ON CONFLICT(normalized_path) DO UPDATE SET file_type=excluded.file_type,size=excluded.size,mtime_ns=excluded.mtime_ns,platform_id=excluded.platform_id,tombstone=0,updated_at=CURRENT_TIMESTAMP`, record.FileID, record.NormalizedPath, record.FileType, record.Size, record.MTimeNS, record.PlatformID)
	return err
}

func (s *ClientStore) TombstoneFile(ctx context.Context, path string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE files SET tombstone=1,updated_at=CURRENT_TIMESTAMP WHERE normalized_path=?`, path)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("file not found: %s", path)
	}
	return nil
}
