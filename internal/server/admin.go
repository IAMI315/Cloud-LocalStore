package server

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
)

// AdminFile is the current server-side view of a path. It is derived from the
// committed operation log, so it remains compatible with databases created by
// older server versions.
type AdminFile struct {
	SpaceID, Path, OperationType, ManifestID, CreatedAt string
	Revision                                            int64
}

type AdminDevice struct {
	DeviceID, SpaceID, Name, Status string
	LastSeen, LastApplied, LastAck  int64
}

type AdminUsage struct {
	DatabaseBytes, ObjectBytes, ObjectCount     int64
	TemporaryBytes, TemporaryCount              int64
	WorkspaceCount, DeviceCount, OperationCount int64
}

type GCReport struct {
	RemovedFiles, RemovedBytes int64
	KeptFiles, KeptBytes       int64
}

// ListAdminFiles returns the latest committed operation for each path.
func (a *App) ListAdminFiles(ctx context.Context, spaceID string) ([]AdminFile, error) {
	query := `SELECT space_id,path_id,operation_type,COALESCE(file_manifest_id,''),revision,created_at
		FROM operations WHERE state='committed' AND (?='' OR space_id=?) ORDER BY space_id,revision`
	rows, err := a.db.QueryContext(ctx, query, spaceID, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	latest := make(map[string]AdminFile)
	for rows.Next() {
		var f AdminFile
		var pathID string
		if err := rows.Scan(&f.SpaceID, &pathID, &f.OperationType, &f.ManifestID, &f.Revision, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.Path = decodeAdminPath(pathID)
		latest[f.SpaceID+"\x00"+f.Path] = f
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]AdminFile, 0, len(latest))
	for _, f := range latest {
		result = append(result, f)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SpaceID == result[j].SpaceID {
			return result[i].Path < result[j].Path
		}
		return result[i].SpaceID < result[j].SpaceID
	})
	return result, nil
}

func decodeAdminPath(value string) string {
	if !strings.HasPrefix(value, "p-") {
		return value
	}
	b, err := hex.DecodeString(strings.TrimPrefix(value, "p-"))
	if err != nil {
		return value
	}
	return string(b)
}

func (a *App) ListAdminDevices(ctx context.Context) ([]AdminDevice, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT device_id,space_id,name,status,last_seen_revision,last_applied_revision,last_ack_revision FROM devices ORDER BY space_id,device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AdminDevice
	for rows.Next() {
		var d AdminDevice
		if err := rows.Scan(&d.DeviceID, &d.SpaceID, &d.Name, &d.Status, &d.LastSeen, &d.LastApplied, &d.LastAck); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (a *App) AdminUsage(ctx context.Context) (AdminUsage, error) {
	var usage AdminUsage
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces`).Scan(&usage.WorkspaceCount); err != nil {
		return usage, err
	}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&usage.DeviceCount); err != nil {
		return usage, err
	}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations`).Scan(&usage.OperationCount); err != nil {
		return usage, err
	}
	if a.cfg.DatabasePath != ":memory:" {
		if info, err := os.Stat(a.cfg.DatabasePath); err == nil {
			usage.DatabaseBytes = info.Size()
		}
	}
	if a.objects == nil {
		return usage, nil
	}
	err := filepath.WalkDir(a.objects.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".upload-") {
			usage.TemporaryCount++
			usage.TemporaryBytes += info.Size()
		} else {
			usage.ObjectCount++
			usage.ObjectBytes += info.Size()
		}
		return nil
	})
	return usage, err
}

func (a *App) CleanupUploadTemps(ctx context.Context) error {
	if a.objects == nil {
		return nil
	}
	return a.objects.CleanupTemp(ctx)
}

// GarbageCollect removes only objects that are not referenced by any prepared
// or committed operation. A grace period protects a recently interrupted
// upload and makes the operation safe to run from the management console.
func (a *App) GarbageCollect(ctx context.Context, grace time.Duration) (GCReport, error) {
	if a.objects == nil {
		return GCReport{}, nil
	}
	// Keep the latest operation for every path, plus operations newer than the
	// slowest active device's acknowledgement cursor. This preserves the data
	// needed by offline clients while allowing acknowledged historical versions
	// to be collected.
	rows, err := a.db.QueryContext(ctx, `SELECT space_id,path_id,operation_type,COALESCE(file_manifest_id,''),revision,state FROM operations ORDER BY space_id,revision`)
	if err != nil {
		return GCReport{}, err
	}
	type operationRef struct {
		spaceID, pathID, operationType, manifestID, state string
		revision                                          int64
	}
	var operations []operationRef
	for rows.Next() {
		var op operationRef
		if err := rows.Scan(&op.spaceID, &op.pathID, &op.operationType, &op.manifestID, &op.revision, &op.state); err != nil {
			rows.Close()
			return GCReport{}, err
		}
		operations = append(operations, op)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GCReport{}, err
	}
	rows.Close()
	acks := make(map[string]int64)
	rows, err = a.db.QueryContext(ctx, `SELECT space_id,MIN(last_ack_revision),COUNT(*) FROM devices WHERE status='active' GROUP BY space_id`)
	if err != nil {
		return GCReport{}, err
	}
	for rows.Next() {
		var spaceID string
		var ack, count int64
		if err := rows.Scan(&spaceID, &ack, &count); err != nil {
			rows.Close()
			return GCReport{}, err
		}
		if count > 0 {
			acks[spaceID] = ack
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return GCReport{}, err
	}
	rows.Close()
	latest := make(map[string]int64)
	for _, op := range operations {
		key := op.spaceID + "\x00" + op.pathID
		if op.revision > latest[key] {
			latest[key] = op.revision
		}
	}
	live := make(map[string]struct{})
	for _, op := range operations {
		if op.manifestID == "" {
			continue
		}
		ack, hasAck := acks[op.spaceID]
		keep := op.state == "prepared" || latest[op.spaceID+"\x00"+op.pathID] == op.revision || !hasAck || op.revision > ack
		if !keep {
			continue
		}
		live[op.manifestID] = struct{}{}
		manifest, err := a.objects.ReadManifest(ctx, domain.ManifestID(op.manifestID))
		if err != nil {
			return GCReport{}, fmt.Errorf("read referenced manifest %s: %w", op.manifestID, err)
		}
		for _, chunk := range manifest.Chunks {
			live[string(chunk)] = struct{}{}
		}
	}
	cutoff := time.Now().Add(-grace)
	var report GCReport
	err = filepath.WalkDir(a.objects.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".upload-") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			report.KeptFiles++
			report.KeptBytes += info.Size()
			return nil
		}
		if _, ok := live[entry.Name()]; ok {
			report.KeptFiles++
			report.KeptBytes += info.Size()
			return nil
		}
		if len(entry.Name()) != 64 {
			return nil
		}
		if _, err := hex.DecodeString(entry.Name()); err != nil {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		report.RemovedFiles++
		report.RemovedBytes += info.Size()
		return nil
	})
	return report, err
}
