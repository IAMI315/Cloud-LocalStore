package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
)

// OpenServer has a separate migration namespace. Revision, device, object and
// journal tables will be added here in the server stages without coupling the
// server database to the client file index schema.
func OpenServer(ctx context.Context, db *sql.DB) (*ServerStore, error) {
	// A single writer connection keeps in-memory SQLite databases reliable in tests
	// and still allows WAL-backed file databases to serialize metadata writes.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return nil, fmt.Errorf("configure sqlite (%s): %w", pragma, err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS server_schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL DEFAULT (datetime('now','localtime')))`); err != nil {
		return nil, fmt.Errorf("create server migration table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspaces (space_id TEXT PRIMARY KEY, quota_bytes INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')))`); err != nil {
		return nil, fmt.Errorf("create workspace table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE workspaces ADD COLUMN quota_bytes INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return nil, fmt.Errorf("migrate workspace quota: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS devices (
		device_id TEXT PRIMARY KEY,
		space_id TEXT NOT NULL REFERENCES workspaces(space_id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('active','revoked')),
		created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
		last_seen_revision INTEGER NOT NULL DEFAULT 0,
		last_applied_revision INTEGER NOT NULL DEFAULT 0,
		last_ack_revision INTEGER NOT NULL DEFAULT 0,
		device_epoch INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return nil, fmt.Errorf("create device table: %w", err)
	}
	// Keep databases created by Phase 3A readable after the cursor columns were
	// introduced. SQLite has no ADD COLUMN IF NOT EXISTS, so ignore duplicate
	// column errors for each compatibility migration.
	for _, stmt := range []string{
		`ALTER TABLE devices ADD COLUMN last_seen_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE devices ADD COLUMN last_applied_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE devices ADD COLUMN last_ack_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE devices ADD COLUMN device_epoch INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE devices ADD COLUMN last_seen_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN last_status_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN last_sync_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE devices ADD COLUMN client_status TEXT NOT NULL DEFAULT 'unknown'`,
		`ALTER TABLE devices ADD COLUMN monitoring INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return nil, fmt.Errorf("migrate device cursor: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE devices ADD COLUMN device_token_hash TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return nil, fmt.Errorf("migrate device token: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS space_revisions (
		space_id TEXT PRIMARY KEY REFERENCES workspaces(space_id) ON DELETE CASCADE,
		current_revision INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return nil, fmt.Errorf("create space revision table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS operations (
		operation_id TEXT PRIMARY KEY,
		space_id TEXT NOT NULL REFERENCES workspaces(space_id) ON DELETE CASCADE,
		origin_device_id TEXT NOT NULL REFERENCES devices(device_id),
		parent_revision INTEGER NOT NULL,
		revision INTEGER NOT NULL DEFAULT 0,
		path_id TEXT NOT NULL,
		operation_type TEXT NOT NULL,
		file_manifest_id TEXT,
		base_file_version_id TEXT,
		device_local_sequence INTEGER NOT NULL,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL CHECK(state IN ('prepared','committed')),
		created_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
	)`); err != nil {
		return nil, fmt.Errorf("create operation table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE operations ADD COLUMN size_bytes INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return nil, fmt.Errorf("migrate operation size: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS space_usage (
		space_id TEXT PRIMARY KEY REFERENCES workspaces(space_id) ON DELETE CASCADE,
		used_bytes INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
	)`); err != nil {
		return nil, fmt.Errorf("create space usage table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS operations_space_revision_idx ON operations(space_id, revision) WHERE revision > 0`); err != nil {
		return nil, fmt.Errorf("create operation revision index: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS operations_space_after_idx ON operations(space_id, revision)`); err != nil {
		return nil, fmt.Errorf("create operation query index: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS devices_space_idx ON devices(space_id)`); err != nil {
		return nil, fmt.Errorf("create device index: %w", err)
	}
	var existingMigrations int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM server_schema_migrations`).Scan(&existingMigrations); err != nil {
		return nil, fmt.Errorf("count server migrations: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO server_schema_migrations(version, name) VALUES (1, 'server_metadata_devices')`); err != nil {
		return nil, fmt.Errorf("record server migration: %w", err)
	}
	if existingMigrations > 0 {
		var migrated int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM server_schema_migrations WHERE version=2`).Scan(&migrated); err != nil {
			return nil, fmt.Errorf("check server timezone migration: %w", err)
		}
		if migrated == 0 {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return nil, fmt.Errorf("begin server timezone migration: %w", err)
			}
			legacyUpdates := []string{
				`UPDATE workspaces SET created_at=datetime(created_at,'utc','localtime') WHERE created_at <> ''`,
				`UPDATE devices SET created_at=datetime(created_at,'utc','localtime'),updated_at=datetime(updated_at,'utc','localtime'),last_seen_at=CASE WHEN last_seen_at='' THEN '' ELSE datetime(last_seen_at,'utc','localtime') END,last_status_at=CASE WHEN last_status_at='' THEN '' ELSE datetime(last_status_at,'utc','localtime') END,last_sync_at=CASE WHEN last_sync_at='' THEN '' ELSE datetime(last_sync_at,'utc','localtime') END`,
				`UPDATE operations SET created_at=datetime(created_at,'utc','localtime') WHERE created_at <> ''`,
				`UPDATE space_usage SET updated_at=datetime(updated_at,'utc','localtime') WHERE updated_at <> ''`,
			}
			for _, stmt := range legacyUpdates {
				if _, err = tx.ExecContext(ctx, stmt); err != nil {
					_ = tx.Rollback()
					return nil, fmt.Errorf("convert server timestamps: %w", err)
				}
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO server_schema_migrations(version,name) VALUES (2,'server_local_timezone_timestamps')`); err != nil {
				_ = tx.Rollback()
				return nil, fmt.Errorf("record server timezone migration: %w", err)
			}
			if err = tx.Commit(); err != nil {
				return nil, fmt.Errorf("commit server timezone migration: %w", err)
			}
		}
	}
	return &ServerStore{DB: db}, nil
}

type ServerStore struct {
	DB                *sql.DB
	Objects           *objectstore.LocalFS
	DefaultQuotaBytes int64
}

type Device struct {
	DeviceID, SpaceID, Name, Status                                     string
	LastSeenAt, LastStatusAt, LastSyncAt, ClientStatus                  string
	Monitoring                                                          bool
	LastSeenRevision, LastAppliedRevision, LastAckRevision, DeviceEpoch int64
}

func HashDeviceToken(token string) string {
	b := sha256.Sum256([]byte(token))
	return hex.EncodeToString(b[:])
}

var ErrNotFound = errors.New("not found")
var ErrRevoked = errors.New("device revoked")
var ErrConflict = errors.New("device belongs to another workspace")
var ErrVersionConflict = errors.New("parent revision conflict")
var ErrRevisionTooOld = errors.New("revision is too old")
var ErrObjectMissing = errors.New("object missing or invalid")
var ErrQuotaExceeded = errors.New("storage quota exceeded")
var ErrQuotaBelowUsed = errors.New("storage quota is below current usage")

const DefaultQuotaBytes int64 = 5 * 1024 * 1024 * 1024
const MaxQuotaBytes int64 = 1 << 50

type Operation struct {
	OperationID, SpaceID, OriginDeviceID string
	ParentRevision, Revision             int64
	PathID, OperationType                string
	FileManifestID, BaseFileVersionID    string
	DeviceLocalSequence                  int64
	SizeBytes                            int64
	State, CreatedAt                     string
}

type DeviceCursor struct {
	DeviceID                                                            string
	LastSeenRevision, LastAppliedRevision, LastAckRevision, DeviceEpoch int64
}

func (s *ServerStore) RegisterDevice(ctx context.Context, spaceID, deviceID, name string) (Device, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, err
	}
	defer tx.Rollback()
	workspaceInsert, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workspaces(space_id) VALUES (?)`, spaceID)
	if err != nil {
		return Device{}, err
	}
	if inserted, rowsErr := workspaceInsert.RowsAffected(); rowsErr == nil && inserted > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO space_usage(space_id,used_bytes) VALUES (?,0)`, spaceID); err != nil {
			return Device{}, err
		}
	}
	var d Device
	err = tx.QueryRowContext(ctx, `SELECT device_id, space_id, name, status FROM devices WHERE device_id = ?`, deviceID).Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status)
	if err == nil {
		if d.SpaceID != spaceID {
			return Device{}, ErrConflict
		}
		if d.Status == "revoked" {
			return d, ErrRevoked
		}
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET name=?, updated_at=datetime('now','localtime') WHERE device_id=?`, name, deviceID); err != nil {
			return Device{}, err
		}
		d.Name = name
	} else if errors.Is(err, sql.ErrNoRows) {
		d = Device{DeviceID: deviceID, SpaceID: spaceID, Name: name, Status: "active"}
		if _, err = tx.ExecContext(ctx, `INSERT INTO devices(device_id, space_id, name, status) VALUES (?, ?, ?, 'active')`, deviceID, spaceID, name); err != nil {
			return Device{}, err
		}
	} else {
		return Device{}, err
	}
	if err = tx.Commit(); err != nil {
		return Device{}, err
	}
	return d, nil
}

// RegisterDeviceWithToken registers or updates a device and returns a fresh
// bearer credential. The hash is persisted so revocation immediately rejects it.
func (s *ServerStore) RegisterDeviceWithToken(ctx context.Context, spaceID, deviceID, name string) (Device, string, error) {
	d, err := s.RegisterDevice(ctx, spaceID, deviceID, name)
	if err != nil {
		return Device{}, "", err
	}
	return s.rotateDeviceToken(ctx, d)
}

// RenameDeviceWithToken keeps the stable device identity while changing its
// display name. This is used when an existing client edits the old device
// field (for example desktop-local -> Lennovo); it must not create a second
// active device record.
func (s *ServerStore) RenameDeviceWithToken(ctx context.Context, spaceID, deviceID, name string) (Device, string, error) {
	var d Device
	err := s.DB.QueryRowContext(ctx, `SELECT device_id,space_id,name,status FROM devices WHERE device_id=? AND space_id=?`, deviceID, spaceID).Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, "", ErrNotFound
	}
	if err != nil {
		return Device{}, "", err
	}
	if d.Status != "active" {
		return d, "", ErrRevoked
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE devices SET name=?,updated_at=datetime('now','localtime') WHERE device_id=? AND space_id=?`, name, deviceID, spaceID); err != nil {
		return Device{}, "", err
	}
	d.Name = name
	return s.rotateDeviceToken(ctx, d)
}

func (s *ServerStore) rotateDeviceToken(ctx context.Context, d Device) (Device, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Device{}, "", err
	}
	token := fmt.Sprintf("%x", b)
	if _, err := s.DB.ExecContext(ctx, `UPDATE devices SET device_token_hash=?, updated_at=datetime('now','localtime') WHERE device_id=?`, HashDeviceToken(token), d.DeviceID); err != nil {
		return Device{}, "", err
	}
	return d, token, nil
}

func (s *ServerStore) AuthenticateDevice(ctx context.Context, deviceID, token string) (Device, error) {
	var d Device
	err := s.DB.QueryRowContext(ctx, `SELECT device_id,space_id,name,status FROM devices WHERE device_id=? AND device_token_hash=?`, deviceID, HashDeviceToken(token)).Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

// TouchDevice records liveness without changing the client-reported state.
// It is intentionally cheap and may be called for every authenticated request.
func (s *ServerStore) TouchDevice(ctx context.Context, spaceID, deviceID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET last_seen_at=datetime('now','localtime') WHERE device_id=? AND space_id=? AND status='active'`, deviceID, spaceID)
	return err
}

// UpdatePresence records an explicit client state transition and returns the
// complete device record for event publication.
func (s *ServerStore) UpdatePresence(ctx context.Context, spaceID, deviceID, clientStatus string, monitoring, syncCompleted bool) (Device, error) {
	if strings.TrimSpace(clientStatus) == "" {
		clientStatus = "running"
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET last_seen_at=datetime('now','localtime'),last_status_at=datetime('now','localtime'),client_status=?,monitoring=?,last_sync_at=CASE WHEN ? THEN datetime('now','localtime') ELSE last_sync_at END WHERE device_id=? AND space_id=? AND status='active'`, clientStatus, boolInt(monitoring), boolInt(syncCompleted), deviceID, spaceID)
	if err != nil {
		return Device{}, err
	}
	return s.GetDevicePresence(ctx, spaceID, deviceID)
}

// MarkDeviceDisconnected clears the lease when the client's event stream is
// closed. The client deliberately does not send an exit request; closing the
// authenticated SSE request is the server-side disconnect signal.
func (s *ServerStore) MarkDeviceDisconnected(ctx context.Context, spaceID, deviceID string) (Device, error) {
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET last_seen_at='',last_status_at=datetime('now','localtime'),client_status='offline',monitoring=0,updated_at=datetime('now','localtime') WHERE device_id=? AND space_id=? AND status='active'`, deviceID, spaceID)
	if err != nil {
		return Device{}, err
	}
	return s.GetDevicePresence(ctx, spaceID, deviceID)
}

func (s *ServerStore) GetDevicePresence(ctx context.Context, spaceID, deviceID string) (Device, error) {
	var d Device
	err := s.DB.QueryRowContext(ctx, `SELECT device_id,space_id,name,status,last_seen_at,last_status_at,last_sync_at,client_status,monitoring,last_seen_revision,last_applied_revision,last_ack_revision,device_epoch FROM devices WHERE device_id=? AND space_id=?`, deviceID, spaceID).Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status, &d.LastSeenAt, &d.LastStatusAt, &d.LastSyncAt, &d.ClientStatus, &d.Monitoring, &d.LastSeenRevision, &d.LastAppliedRevision, &d.LastAckRevision, &d.DeviceEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

func (s *ServerStore) RevokeDevice(ctx context.Context, spaceID, deviceID string) (Device, error) {
	var d Device
	err := s.DB.QueryRowContext(ctx, `SELECT device_id, space_id, name, status FROM devices WHERE device_id=? AND space_id=?`, deviceID, spaceID).Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, err
	}
	if d.Status == "active" {
		if _, err = s.DB.ExecContext(ctx, `UPDATE devices SET status='revoked', device_token_hash='', updated_at=datetime('now','localtime') WHERE device_id=?`, deviceID); err != nil {
			return Device{}, err
		}
		d.Status = "revoked"
	}
	return d, nil
}

func (s *ServerStore) GetDevice(ctx context.Context, spaceID, deviceID string) (Device, error) {
	var d Device
	err := s.DB.QueryRowContext(ctx, `SELECT device_id, space_id, name, status FROM devices WHERE device_id=? AND space_id=?`, deviceID, spaceID).Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

func (s *ServerStore) GetActiveDevice(ctx context.Context, deviceID string) (Device, error) {
	var d Device
	err := s.DB.QueryRowContext(ctx, `SELECT device_id, space_id, name, status FROM devices WHERE device_id=? AND status='active'`, deviceID).Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

func (s *ServerStore) WorkspaceExists(ctx context.Context, spaceID string) (bool, error) {
	var value int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE space_id=?`, spaceID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *ServerStore) WorkspaceState(ctx context.Context, spaceID string) ([]Device, error) {
	var exists int
	if err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE space_id=?`, spaceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT device_id, space_id, name, status,last_seen_at,last_status_at,last_sync_at,client_status,monitoring,last_seen_revision,last_applied_revision,last_ack_revision,device_epoch FROM devices WHERE space_id=? AND status='active' ORDER BY device_id`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status, &d.LastSeenAt, &d.LastStatusAt, &d.LastSyncAt, &d.ClientStatus, &d.Monitoring, &d.LastSeenRevision, &d.LastAppliedRevision, &d.LastAckRevision, &d.DeviceEpoch); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *ServerStore) CurrentRevision(ctx context.Context, spaceID string) (int64, error) {
	var revision int64
	err := s.DB.QueryRowContext(ctx, `SELECT current_revision FROM space_revisions WHERE space_id=?`, spaceID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func (s *ServerStore) CheckWritable(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS server_ready_probe (id INTEGER PRIMARY KEY, touched_at TEXT)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO server_ready_probe(id,touched_at) VALUES (1,datetime('now','localtime'))`); err != nil {
		return err
	}
	return tx.Commit()
}

func scanOperation(scanner interface{ Scan(...any) error }) (Operation, error) {
	var op Operation
	var manifest, base sql.NullString
	err := scanner.Scan(&op.OperationID, &op.SpaceID, &op.OriginDeviceID, &op.ParentRevision, &op.Revision, &op.PathID, &op.OperationType, &manifest, &base, &op.DeviceLocalSequence, &op.SizeBytes, &op.State, &op.CreatedAt)
	if manifest.Valid {
		op.FileManifestID = manifest.String
	}
	if base.Valid {
		op.BaseFileVersionID = base.String
	}
	return op, err
}

func (s *ServerStore) PrepareOperation(ctx context.Context, spaceID, deviceID string, op Operation) (Operation, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM devices WHERE device_id=? AND space_id=?`, deviceID, spaceID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	} else if err != nil {
		return Operation{}, err
	}
	if status != "active" {
		return Operation{}, ErrRevoked
	}
	var existing Operation
	existing, err = scanOperation(tx.QueryRowContext(ctx, `SELECT operation_id, space_id, origin_device_id, parent_revision, revision, path_id, operation_type, file_manifest_id, base_file_version_id, device_local_sequence, size_bytes, state, created_at FROM operations WHERE operation_id=?`, op.OperationID))
	if err == nil {
		if existing.SpaceID != spaceID || existing.OriginDeviceID != deviceID {
			return Operation{}, ErrConflict
		}
		if existing.ParentRevision != op.ParentRevision || existing.PathID != op.PathID || existing.OperationType != op.OperationType || existing.FileManifestID != op.FileManifestID || existing.BaseFileVersionID != op.BaseFileVersionID || existing.DeviceLocalSequence != op.DeviceLocalSequence {
			return Operation{}, ErrConflict
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, err
	}
	if op.OriginDeviceID == "" {
		op.OriginDeviceID = deviceID
	}
	op.SpaceID, op.State, op.Revision = spaceID, "prepared", 0
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations(operation_id,space_id,origin_device_id,parent_revision,path_id,operation_type,file_manifest_id,base_file_version_id,device_local_sequence,size_bytes,state) VALUES(?,?,?,?,?,?,?,?,?,?, 'prepared')`, op.OperationID, spaceID, deviceID, op.ParentRevision, op.PathID, op.OperationType, nullable(op.FileManifestID), nullable(op.BaseFileVersionID), op.DeviceLocalSequence, 0); err != nil {
		return Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *ServerStore) CommitOperation(ctx context.Context, spaceID, deviceID, operationID string) (Operation, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM devices WHERE device_id=? AND space_id=?`, deviceID, spaceID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	} else if err != nil {
		return Operation{}, err
	}
	if status != "active" {
		return Operation{}, ErrRevoked
	}
	var op Operation
	op, err = scanOperation(tx.QueryRowContext(ctx, `SELECT operation_id, space_id, origin_device_id, parent_revision, revision, path_id, operation_type, file_manifest_id, base_file_version_id, device_local_sequence, size_bytes, state, created_at FROM operations WHERE operation_id=?`, operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	if op.SpaceID != spaceID || op.OriginDeviceID != deviceID {
		return Operation{}, ErrConflict
	}
	if op.State == "committed" {
		return op, nil
	}
	if op.FileManifestID != "" && isContentHash(op.FileManifestID) && s.Objects != nil {
		if err := s.validateManifest(ctx, op.FileManifestID); err != nil {
			return Operation{}, err
		}
	}
	previousSize, err := latestPathSizeTx(ctx, tx, spaceID, op.PathID)
	if err != nil {
		return Operation{}, err
	}
	newSize := previousSize
	if op.OperationType == "delete" {
		newSize = 0
	} else if isContentHash(op.FileManifestID) && s.Objects != nil {
		newSize, err = s.manifestSize(ctx, op.FileManifestID)
		if err != nil {
			return Operation{}, err
		}
	}
	if newSize < 0 {
		return Operation{}, ErrObjectMissing
	}
	used, err := ensureSpaceUsageTx(ctx, tx, s, spaceID)
	if err != nil {
		return Operation{}, err
	}
	delta := newSize - previousSize
	quotaBytes, err := workspaceQuotaTx(ctx, tx, spaceID, s.defaultQuota())
	if err != nil {
		return Operation{}, err
	}
	if delta > 0 && used > quotaBytes-delta {
		return Operation{}, ErrQuotaExceeded
	}
	newUsed := used + delta
	if newUsed < 0 {
		newUsed = 0
	}
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT current_revision FROM space_revisions WHERE space_id=?`, spaceID).Scan(&current); errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO space_revisions(space_id,current_revision) VALUES(?,0)`, spaceID); err != nil {
			return Operation{}, err
		}
		current = 0
	} else if err != nil {
		return Operation{}, err
	}
	if op.ParentRevision != current {
		return Operation{}, ErrVersionConflict
	}
	newRevision := current + 1
	if _, err = tx.ExecContext(ctx, `UPDATE space_revisions SET current_revision=? WHERE space_id=?`, newRevision, spaceID); err != nil {
		return Operation{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET revision=?,size_bytes=?,state='committed' WHERE operation_id=? AND state='prepared'`, newRevision, newSize, operationID); err != nil {
		return Operation{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO space_usage(space_id,used_bytes,updated_at) VALUES(?,?,datetime('now','localtime')) ON CONFLICT(space_id) DO UPDATE SET used_bytes=excluded.used_bytes,updated_at=datetime('now','localtime')`, spaceID, newUsed); err != nil {
		return Operation{}, err
	}
	op.Revision, op.State = newRevision, "committed"
	op.SizeBytes = newSize
	if err = tx.Commit(); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func isContentHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// validateManifest verifies the manifest object itself and every referenced
// chunk before an operation can become visible in the revision log.
func (s *ServerStore) validateManifest(ctx context.Context, id string) error {
	b, err := s.Objects.ReadObject(ctx, domain.ChunkHash(id))
	if err != nil {
		return ErrObjectMissing
	}
	m, err := objectstore.UnmarshalManifest(b)
	if err != nil {
		return ErrObjectMissing
	}
	for _, hash := range m.Chunks {
		ok, err := s.Objects.Has(ctx, hash)
		if err != nil || !ok {
			return ErrObjectMissing
		}
	}
	return nil
}

func (s *ServerStore) manifestSize(ctx context.Context, id string) (int64, error) {
	if !isContentHash(id) || s.Objects == nil {
		return 0, ErrObjectMissing
	}
	b, err := s.Objects.ReadObject(ctx, domain.ChunkHash(id))
	if err != nil {
		return 0, ErrObjectMissing
	}
	m, err := objectstore.UnmarshalManifest(b)
	if err != nil || m.Size < 0 {
		return 0, ErrObjectMissing
	}
	return m.Size, nil
}

func (s *ServerStore) defaultQuota() int64 {
	if s.DefaultQuotaBytes > 0 {
		return s.DefaultQuotaBytes
	}
	return DefaultQuotaBytes
}

func workspaceQuotaTx(ctx context.Context, tx *sql.Tx, spaceID string, fallback int64) (int64, error) {
	var configured int64
	if err := tx.QueryRowContext(ctx, `SELECT quota_bytes FROM workspaces WHERE space_id=?`, spaceID).Scan(&configured); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if configured <= 0 {
		configured = fallback
	}
	return configured, nil
}

func latestPathSizeTx(ctx context.Context, tx *sql.Tx, spaceID, pathID string) (int64, error) {
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM operations WHERE space_id=? AND path_id=? AND state='committed' ORDER BY revision DESC LIMIT 1`, spaceID, pathID).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return size, err
}

func ensureSpaceUsageTx(ctx context.Context, tx *sql.Tx, s *ServerStore, spaceID string) (int64, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE space_id=?`, spaceID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	var used int64
	err := tx.QueryRowContext(ctx, `SELECT used_bytes FROM space_usage WHERE space_id=?`, spaceID).Scan(&used)
	if err == nil {
		return used, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT operation_id,path_id,operation_type,file_manifest_id,size_bytes FROM operations WHERE space_id=? AND state='committed' ORDER BY revision`, spaceID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	latest := make(map[string]int64)
	type rebuiltOperation struct {
		id   string
		size int64
	}
	rebuilt := make([]rebuiltOperation, 0)
	for rows.Next() {
		var operationID, pathID, operationType string
		var manifest sql.NullString
		var size int64
		if err := rows.Scan(&operationID, &pathID, &operationType, &manifest, &size); err != nil {
			return 0, err
		}
		if operationType == "delete" {
			latest[pathID] = 0
			rebuilt = append(rebuilt, rebuiltOperation{id: operationID, size: 0})
			continue
		}
		if manifest.Valid && manifest.String != "" && s.Objects != nil {
			if manifestSize, manifestErr := s.manifestSize(ctx, manifest.String); manifestErr == nil {
				size = manifestSize
			}
		}
		latest[pathID] = size
		rebuilt = append(rebuilt, rebuiltOperation{id: operationID, size: size})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, operation := range rebuilt {
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET size_bytes=? WHERE operation_id=?`, operation.size, operation.id); err != nil {
			return 0, err
		}
	}
	for _, size := range latest {
		if size > 0 {
			used += size
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO space_usage(space_id,used_bytes,updated_at) VALUES(?,?,datetime('now','localtime'))`, spaceID, used); err != nil {
		return 0, err
	}
	return used, nil
}

// SpaceQuota returns the effective quota and logical manifest usage.
func (s *ServerStore) SpaceQuota(ctx context.Context, spaceID string) (int64, int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	quota, err := workspaceQuotaTx(ctx, tx, spaceID, s.defaultQuota())
	if err != nil {
		return 0, 0, err
	}
	used, err := ensureSpaceUsageTx(ctx, tx, s, spaceID)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return quota, used, nil
}

// SetSpaceQuota stores a workspace override. Zero restores the server default.
func (s *ServerStore) SetSpaceQuota(ctx context.Context, spaceID string, quotaBytes int64) (int64, int64, error) {
	if quotaBytes < 0 || quotaBytes > MaxQuotaBytes {
		return 0, 0, fmt.Errorf("quota must be between 0 and %d", MaxQuotaBytes)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	used, err := ensureSpaceUsageTx(ctx, tx, s, spaceID)
	if err != nil {
		return 0, 0, err
	}
	effective := quotaBytes
	if effective == 0 {
		effective = s.defaultQuota()
	}
	if effective < used {
		return 0, used, ErrQuotaBelowUsed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET quota_bytes=? WHERE space_id=?`, quotaBytes, spaceID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return effective, used, nil
}

// RebuildSpaceUsage replays committed operations in revision order.
func (s *ServerStore) RebuildSpaceUsage(ctx context.Context, spaceID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM space_usage WHERE space_id=?`, spaceID); err != nil {
		return err
	}
	_, err = ensureSpaceUsageTx(ctx, tx, s, spaceID)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *ServerStore) ListOperations(ctx context.Context, spaceID string, after int64) ([]Operation, int64, error) {
	if after < 0 {
		return nil, 0, ErrRevisionTooOld
	}
	var exists int
	if err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE space_id=?`, spaceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNotFound
	} else if err != nil {
		return nil, 0, err
	}
	var current int64
	if err := s.DB.QueryRowContext(ctx, `SELECT current_revision FROM space_revisions WHERE space_id=?`, spaceID).Scan(&current); errors.Is(err, sql.ErrNoRows) {
		return []Operation{}, 0, nil
	} else if err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT operation_id, space_id, origin_device_id, parent_revision, revision, path_id, operation_type, file_manifest_id, base_file_version_id, device_local_sequence, size_bytes, state, created_at FROM operations WHERE space_id=? AND state='committed' AND revision>? ORDER BY revision LIMIT 1000`, spaceID, after)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]Operation, 0)
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, op)
	}
	return result, current, rows.Err()
}

func (s *ServerStore) Ack(ctx context.Context, spaceID, deviceID string, seen, applied, ack, epoch int64) (DeviceCursor, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return DeviceCursor{}, err
	}
	defer tx.Rollback()
	var d DeviceCursor
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status,last_seen_revision,last_applied_revision,last_ack_revision,device_epoch FROM devices WHERE device_id=? AND space_id=?`, deviceID, spaceID).Scan(&status, &d.LastSeenRevision, &d.LastAppliedRevision, &d.LastAckRevision, &d.DeviceEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceCursor{}, ErrNotFound
	}
	if err != nil {
		return DeviceCursor{}, err
	}
	if status != "active" {
		return DeviceCursor{}, ErrRevoked
	}
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT current_revision FROM space_revisions WHERE space_id=?`, spaceID).Scan(&current); errors.Is(err, sql.ErrNoRows) {
		current = 0
	} else if err != nil {
		return DeviceCursor{}, err
	}
	if ack > current || seen > current || applied > current {
		return DeviceCursor{}, ErrVersionConflict
	}
	if seen < d.LastSeenRevision {
		seen = d.LastSeenRevision
	}
	if applied < d.LastAppliedRevision {
		applied = d.LastAppliedRevision
	}
	if ack < d.LastAckRevision {
		ack = d.LastAckRevision
	}
	if epoch < d.DeviceEpoch {
		epoch = d.DeviceEpoch
	}
	if applied > seen {
		seen = applied
	}
	if ack > applied {
		applied = ack
		if seen < applied {
			seen = applied
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET last_seen_revision=?,last_applied_revision=?,last_ack_revision=?,device_epoch=?,updated_at=datetime('now','localtime') WHERE device_id=? AND status='active'`, seen, applied, ack, epoch, deviceID); err != nil {
		return DeviceCursor{}, err
	}
	if err = tx.Commit(); err != nil {
		return DeviceCursor{}, err
	}
	d.DeviceID, d.LastSeenRevision, d.LastAppliedRevision, d.LastAckRevision, d.DeviceEpoch = deviceID, seen, applied, ack, epoch
	return d, nil
}
