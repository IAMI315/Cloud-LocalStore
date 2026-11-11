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
	"time"

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
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS server_schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return nil, fmt.Errorf("create server migration table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspaces (space_id TEXT PRIMARY KEY, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return nil, fmt.Errorf("create workspace table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS devices (
		device_id TEXT PRIMARY KEY,
		space_id TEXT NOT NULL REFERENCES workspaces(space_id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('active','revoked')),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
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
		state TEXT NOT NULL CHECK(state IN ('prepared','committed')),
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return nil, fmt.Errorf("create operation table: %w", err)
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
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO server_schema_migrations(version, name) VALUES (1, 'server_metadata_devices')`); err != nil {
		return nil, fmt.Errorf("record server migration: %w", err)
	}
	return &ServerStore{DB: db}, nil
}

type ServerStore struct {
	DB      *sql.DB
	Objects *objectstore.LocalFS
}

type Device struct{ DeviceID, SpaceID, Name, Status string }

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

type Operation struct {
	OperationID, SpaceID, OriginDeviceID string
	ParentRevision, Revision             int64
	PathID, OperationType                string
	FileManifestID, BaseFileVersionID    string
	DeviceLocalSequence                  int64
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
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO workspaces(space_id) VALUES (?)`, spaceID); err != nil {
		return Device{}, err
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
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET name=?, updated_at=CURRENT_TIMESTAMP WHERE device_id=?`, name, deviceID); err != nil {
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
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Device{}, "", err
	}
	token := fmt.Sprintf("%x", b)
	if _, err := s.DB.ExecContext(ctx, `UPDATE devices SET device_token_hash=?, updated_at=? WHERE device_id=?`, HashDeviceToken(token), time.Now().UTC(), deviceID); err != nil {
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
		if _, err = s.DB.ExecContext(ctx, `UPDATE devices SET status='revoked', device_token_hash='', updated_at=CURRENT_TIMESTAMP WHERE device_id=?`, deviceID); err != nil {
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
	rows, err := s.DB.QueryContext(ctx, `SELECT device_id, space_id, name, status FROM devices WHERE space_id=? ORDER BY device_id`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
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
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO server_ready_probe(id,touched_at) VALUES (1,CURRENT_TIMESTAMP)`); err != nil {
		return err
	}
	return tx.Commit()
}

func scanOperation(scanner interface{ Scan(...any) error }) (Operation, error) {
	var op Operation
	var manifest, base sql.NullString
	err := scanner.Scan(&op.OperationID, &op.SpaceID, &op.OriginDeviceID, &op.ParentRevision, &op.Revision, &op.PathID, &op.OperationType, &manifest, &base, &op.DeviceLocalSequence, &op.State, &op.CreatedAt)
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
	existing, err = scanOperation(tx.QueryRowContext(ctx, `SELECT operation_id, space_id, origin_device_id, parent_revision, revision, path_id, operation_type, file_manifest_id, base_file_version_id, device_local_sequence, state, created_at FROM operations WHERE operation_id=?`, op.OperationID))
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations(operation_id,space_id,origin_device_id,parent_revision,path_id,operation_type,file_manifest_id,base_file_version_id,device_local_sequence,state) VALUES(?,?,?,?,?,?,?,?,?,'prepared')`, op.OperationID, spaceID, deviceID, op.ParentRevision, op.PathID, op.OperationType, nullable(op.FileManifestID), nullable(op.BaseFileVersionID), op.DeviceLocalSequence); err != nil {
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
	op, err = scanOperation(tx.QueryRowContext(ctx, `SELECT operation_id, space_id, origin_device_id, parent_revision, revision, path_id, operation_type, file_manifest_id, base_file_version_id, device_local_sequence, state, created_at FROM operations WHERE operation_id=?`, operationID))
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
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET revision=?,state='committed' WHERE operation_id=? AND state='prepared'`, newRevision, operationID); err != nil {
		return Operation{}, err
	}
	op.Revision, op.State = newRevision, "committed"
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
	rows, err := s.DB.QueryContext(ctx, `SELECT operation_id, space_id, origin_device_id, parent_revision, revision, path_id, operation_type, file_manifest_id, base_file_version_id, device_local_sequence, state, created_at FROM operations WHERE space_id=? AND state='committed' AND revision>? ORDER BY revision LIMIT 1000`, spaceID, after)
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
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET last_seen_revision=?,last_applied_revision=?,last_ack_revision=?,device_epoch=?,updated_at=CURRENT_TIMESTAMP WHERE device_id=? AND status='active'`, seen, applied, ack, epoch, deviceID); err != nil {
		return DeviceCursor{}, err
	}
	if err = tx.Commit(); err != nil {
		return DeviceCursor{}, err
	}
	d.DeviceID, d.LastSeenRevision, d.LastAppliedRevision, d.LastAckRevision, d.DeviceEpoch = deviceID, seen, applied, ack, epoch
	return d, nil
}
