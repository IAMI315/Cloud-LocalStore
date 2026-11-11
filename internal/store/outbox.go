package store

import (
	"context"
	"database/sql"
)

type SyncOperation struct {
	OperationID, SpaceID, NormalizedPath, ManifestID, OperationType, FileVersionID, State string
	ParentRevision, Revision                                                              int64
}

type Tombstone struct {
	Path, VersionID, ManifestID string
}

type PendingRenameDelete struct {
	DeleteID, Path, VersionID, ManifestID string
}

func (s *ClientStore) FindSyncOperation(ctx context.Context, spaceID, path, manifestID string) (SyncOperation, error) {
	return s.FindSyncOperationByType(ctx, spaceID, path, manifestID, "update")
}

func (s *ClientStore) FindSyncOperationByType(ctx context.Context, spaceID, path, manifestID, operationType string) (SyncOperation, error) {
	var op SyncOperation
	err := s.DB.QueryRowContext(ctx, `SELECT operation_id,space_id,normalized_path,manifest_id,operation_type,file_version_id,parent_revision,state,revision FROM sync_outbox WHERE space_id=? AND normalized_path=? AND manifest_id=? AND operation_type=? ORDER BY revision DESC,updated_at DESC LIMIT 1`, spaceID, path, manifestID, operationType).Scan(&op.OperationID, &op.SpaceID, &op.NormalizedPath, &op.ManifestID, &op.OperationType, &op.FileVersionID, &op.ParentRevision, &op.State, &op.Revision)
	return op, err
}

func (s *ClientStore) FindSyncOperationByVersion(ctx context.Context, spaceID, path, manifestID, operationType, fileVersionID string) (SyncOperation, error) {
	var op SyncOperation
	err := s.DB.QueryRowContext(ctx, `SELECT operation_id,space_id,normalized_path,manifest_id,operation_type,file_version_id,parent_revision,state,revision FROM sync_outbox WHERE space_id=? AND normalized_path=? AND manifest_id=? AND operation_type=? AND file_version_id=?`, spaceID, path, manifestID, operationType, fileVersionID).Scan(&op.OperationID, &op.SpaceID, &op.NormalizedPath, &op.ManifestID, &op.OperationType, &op.FileVersionID, &op.ParentRevision, &op.State, &op.Revision)
	return op, err
}

// FindLegacyUpdateForVersion reuses an old outbox update only when its path
// has not subsequently been deleted. A later delete indicates that the same
// path was recreated and must receive a new operation id even if its content
// is identical.
func (s *ClientStore) FindLegacyUpdateForVersion(ctx context.Context, spaceID, path, manifestID, fileVersionID string) (SyncOperation, error) {
	var op SyncOperation
	err := s.DB.QueryRowContext(ctx, `SELECT operation_id,space_id,normalized_path,manifest_id,operation_type,file_version_id,parent_revision,state,revision FROM sync_outbox WHERE space_id=? AND normalized_path=? AND manifest_id=? AND operation_type='update' AND file_version_id='' ORDER BY revision DESC,updated_at DESC LIMIT 1`, spaceID, path, manifestID).Scan(&op.OperationID, &op.SpaceID, &op.NormalizedPath, &op.ManifestID, &op.OperationType, &op.FileVersionID, &op.ParentRevision, &op.State, &op.Revision)
	if err != nil {
		return op, err
	}
	var deletes int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM sync_outbox WHERE space_id=? AND normalized_path=? AND operation_type='delete' AND state='committed' AND revision>?`, spaceID, path, op.Revision).Scan(&deletes); err != nil {
		return SyncOperation{}, err
	}
	if deletes != 0 {
		return SyncOperation{}, sql.ErrNoRows
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE sync_outbox SET file_version_id=?,updated_at=CURRENT_TIMESTAMP WHERE operation_id=?`, fileVersionID, op.OperationID); err != nil {
		return SyncOperation{}, err
	}
	op.FileVersionID = fileVersionID
	return op, nil
}

// FindSyncOperationByID supports reading outbox entries written by older
// clients whose logical manifest key differed from the current delete key.
// The operation ID is the server's idempotency key, so it is the stable
// compatibility lookup when the path/manifest representation has changed.
func (s *ClientStore) FindSyncOperationByID(ctx context.Context, operationID string) (SyncOperation, error) {
	var op SyncOperation
	err := s.DB.QueryRowContext(ctx, `SELECT operation_id,space_id,normalized_path,manifest_id,operation_type,file_version_id,parent_revision,state,revision FROM sync_outbox WHERE operation_id=?`, operationID).Scan(&op.OperationID, &op.SpaceID, &op.NormalizedPath, &op.ManifestID, &op.OperationType, &op.FileVersionID, &op.ParentRevision, &op.State, &op.Revision)
	return op, err
}

func (s *ClientStore) ListTombstonePaths(ctx context.Context) ([]string, error) {
	tombstones, err := s.ListTombstones(ctx)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(tombstones))
	for _, tombstone := range tombstones {
		paths = append(paths, tombstone.Path)
	}
	return paths, nil
}

func (s *ClientStore) ListTombstones(ctx context.Context) ([]Tombstone, error) {
	rows, err := s.DB.QueryContext(ctx, `
SELECT f.normalized_path, COALESCE(f.current_version_id,''), COALESCE(v.manifest_id,'')
FROM files f
LEFT JOIN file_versions v ON v.version_id=f.current_version_id
WHERE f.file_type='regular' AND f.tombstone=1
ORDER BY f.normalized_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tombstones []Tombstone
	for rows.Next() {
		var tombstone Tombstone
		if err := rows.Scan(&tombstone.Path, &tombstone.VersionID, &tombstone.ManifestID); err != nil {
			return nil, err
		}
		tombstones = append(tombstones, tombstone)
	}
	return tombstones, rows.Err()
}

func (s *ClientStore) ListPendingRenameDeletes(ctx context.Context) ([]PendingRenameDelete, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT delete_id,normalized_path,version_id,manifest_id FROM pending_rename_deletes ORDER BY created_at,delete_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []PendingRenameDelete
	for rows.Next() {
		var item PendingRenameDelete
		if err := rows.Scan(&item.DeleteID, &item.Path, &item.VersionID, &item.ManifestID); err != nil {
			return nil, err
		}
		pending = append(pending, item)
	}
	return pending, rows.Err()
}

func (s *ClientStore) RemovePendingRenameDelete(ctx context.Context, deleteID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM pending_rename_deletes WHERE delete_id=?`, deleteID)
	return err
}

func (s *ClientStore) SaveSyncOperation(ctx context.Context, op SyncOperation) error {
	if op.OperationType == "" {
		op.OperationType = "update"
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sync_outbox(operation_id,space_id,normalized_path,manifest_id,operation_type,file_version_id,parent_revision,state,revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP) ON CONFLICT(operation_id) DO UPDATE SET operation_type=excluded.operation_type,file_version_id=excluded.file_version_id,parent_revision=excluded.parent_revision,state=excluded.state,revision=excluded.revision,updated_at=CURRENT_TIMESTAMP`, op.OperationID, op.SpaceID, op.NormalizedPath, op.ManifestID, op.OperationType, op.FileVersionID, op.ParentRevision, op.State, op.Revision)
	return err
}
