package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type SyncState struct {
	SpaceID             string
	LastSeenRevision    int64
	LastAppliedRevision int64
	LastAckRevision     int64
	DeviceEpoch         int64
}

func (s *ClientStore) GetSyncState(ctx context.Context, spaceID string) (SyncState, error) {
	var st SyncState
	st.SpaceID = spaceID
	err := s.DB.QueryRowContext(ctx, `SELECT last_seen_revision,last_applied_revision,last_ack_revision,device_epoch FROM sync_state WHERE space_id=?`, spaceID).Scan(&st.LastSeenRevision, &st.LastAppliedRevision, &st.LastAckRevision, &st.DeviceEpoch)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, nil
		}
		return st, fmt.Errorf("read sync state: %w", err)
	}
	return st, nil
}

func (s *ClientStore) SaveSyncState(ctx context.Context, st SyncState) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sync_state(space_id,last_seen_revision,last_applied_revision,last_ack_revision,device_epoch,updated_at) VALUES(?,?,?,?,?,datetime('now','localtime')) ON CONFLICT(space_id) DO UPDATE SET last_seen_revision=excluded.last_seen_revision,last_applied_revision=excluded.last_applied_revision,last_ack_revision=excluded.last_ack_revision,device_epoch=excluded.device_epoch,updated_at=datetime('now','localtime')`, st.SpaceID, st.LastSeenRevision, st.LastAppliedRevision, st.LastAckRevision, st.DeviceEpoch)
	return err
}
