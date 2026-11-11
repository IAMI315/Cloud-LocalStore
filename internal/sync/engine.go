package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
	"github.com/IAMI315/Cloud-LocalStore/internal/store"
)

type Remote interface {
	Exists(context.Context, []domain.ChunkHash) (map[domain.ChunkHash]bool, error)
	Upload(context.Context, domain.ChunkHash, io.Reader) error
	Download(context.Context, domain.ChunkHash) (io.ReadCloser, error)
	PrepareOperation(context.Context, protocol.OperationPrepareRequest) (protocol.Operation, error)
	CommitOperation(context.Context, protocol.OperationCommitRequest) (protocol.Operation, error)
	ListOperations(context.Context, int64) (protocol.OperationListResponse, error)
	Ack(context.Context, protocol.AckRequest) (protocol.AckResponse, error)
}

type Engine struct {
	Root      string
	SpaceID   string
	DeviceID  string
	Objects   *objectstore.LocalFS
	Index     *store.ClientStore
	Remote    Remote
	ChunkSize int64
	mu        sync.Mutex
}

func NewEngine(root, spaceID, deviceID string, objects *objectstore.LocalFS, index *store.ClientStore, remote Remote) (*Engine, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(spaceID) == "" || strings.TrimSpace(deviceID) == "" || objects == nil || index == nil || remote == nil {
		return nil, fmt.Errorf("root, space, device, objects, index and remote are required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Engine{Root: filepath.Clean(root), SpaceID: spaceID, DeviceID: deviceID, Objects: objects, Index: index, Remote: remote, ChunkSize: domain.DefaultChunkSize}, nil
}

// Sync is serialized so watcher/timer/manual triggers may safely call it at
// the same time. A cancelled or failed run leaves the cursor before the
// unfinished operation and is therefore safe to retry.
func (e *Engine) Sync(ctx context.Context) error {
	return e.sync(ctx, nil)
}

// SyncWithScan pulls and applies remote operations first, then invokes scan
// before local changes are pushed. The callback is deliberately inside the
// engine lock so a watcher cannot interleave another sync pass.
func (e *Engine) SyncWithScan(ctx context.Context, scan func(context.Context) error) error {
	return e.sync(ctx, scan)
}

func (e *Engine) sync(ctx context.Context, scan func(context.Context) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := e.Index.GetSyncState(ctx, e.SpaceID)
	if err != nil {
		return err
	}
	list, err := e.Remote.ListOperations(ctx, state.LastAppliedRevision)
	if err != nil {
		return err
	}
	state.LastSeenRevision = list.CurrentRevision
	for _, op := range list.Operations {
		apply, err := e.shouldApplyOperation(ctx, op)
		if err != nil {
			return err
		}
		if apply {
			if err := e.applyOperation(ctx, op); err != nil {
				return err
			}
		}
		state.LastAppliedRevision = op.Revision
		if err := e.Index.SaveSyncState(ctx, state); err != nil {
			return err
		}
	}
	if state.LastAppliedRevision > state.LastAckRevision {
		ack, err := e.Remote.Ack(ctx, protocol.AckRequest{AckRevision: state.LastAppliedRevision, LastSeenRevision: state.LastSeenRevision, LastAppliedRevision: state.LastAppliedRevision, DeviceEpoch: state.DeviceEpoch})
		if err != nil {
			return err
		}
		state.LastSeenRevision, state.LastAppliedRevision, state.LastAckRevision, state.DeviceEpoch = ack.LastSeenRevision, ack.LastAppliedRevision, ack.LastAckRevision, ack.DeviceEpoch
		if err := e.Index.SaveSyncState(ctx, state); err != nil {
			return err
		}
	}
	if scan != nil {
		if err := scan(ctx); err != nil {
			return err
		}
	}
	return e.pushLocal(ctx, state.LastSeenRevision)
}

// shouldApplyOperation treats a device's own operation as already applied
// only when the local filesystem and index still contain that exact state.
// This lets a client recover a file after local data loss or an interrupted
// restore while retaining the fast path for ordinary acknowledgements.
func (e *Engine) shouldApplyOperation(ctx context.Context, op protocol.Operation) (bool, error) {
	if op.OriginDeviceID != e.DeviceID {
		return true, nil
	}
	path, err := decodePathID(op.PathID)
	if err != nil {
		return false, err
	}
	target, err := e.safePath(path)
	if err != nil {
		return false, err
	}
	state, err := e.localPathState(ctx, path)
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(target)
	if op.OperationType == string(domain.OperationDelete) {
		return !errors.Is(statErr, os.ErrNotExist) || !state.Tombstone, nil
	}
	return !state.Found || state.Tombstone || state.ManifestID != op.FileManifestID || statErr != nil, nil
}

// RunOnce is an explicit alias for callers that model synchronization as a
// retryable job rather than a long-running loop.
func (e *Engine) RunOnce(ctx context.Context) error { return e.Sync(ctx) }

func (e *Engine) applyOperation(ctx context.Context, op protocol.Operation) error {
	path, err := decodePathID(op.PathID)
	if err != nil {
		return err
	}
	target, err := e.safePath(path)
	if err != nil {
		return err
	}
	local, err := e.localPathState(ctx, path)
	if err != nil {
		return err
	}
	if op.OperationType == string(domain.OperationDelete) {
		dirty, err := e.localPathDirty(ctx, local)
		if err != nil {
			return err
		}
		if dirty {
			if err := e.preserveConflict(path, target, op.Revision); err != nil {
				return err
			}
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
		if local.Found {
			if err := e.Index.TombstoneFile(ctx, path); err != nil {
				return err
			}
		}
		return e.Index.SaveSyncOperation(ctx, store.SyncOperation{
			OperationID: op.OperationID, SpaceID: e.SpaceID, NormalizedPath: path,
			ManifestID: local.ManifestID, OperationType: "delete", ParentRevision: op.ParentRevision,
			FileVersionID: local.VersionID, State: op.State, Revision: op.Revision,
		})
	}
	if op.FileManifestID == "" {
		return fmt.Errorf("operation %s has no manifest", op.OperationID)
	}
	mid := domain.ManifestID(op.FileManifestID)
	r, err := e.Remote.Download(ctx, domain.ChunkHash(mid))
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(domain.DefaultChunkSize)+1))
	closeErr := r.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if domain.ManifestID(objectstore.Hash(b)) != mid {
		return fmt.Errorf("manifest hash mismatch: %s", mid)
	}
	m, err := objectstore.UnmarshalManifest(b)
	if err != nil {
		return err
	}
	if _, err := e.Objects.PutManifest(ctx, m); err != nil {
		return err
	}
	if _, err := e.Index.SaveManifest(ctx, m); err != nil {
		return err
	}
	for _, hash := range m.Chunks {
		ok, err := e.Objects.Has(ctx, hash)
		if err != nil {
			return err
		}
		if ok {
			if err := e.Objects.Verify(ctx, hash); err != nil {
				return err
			}
			continue
		}
		r, err := e.Remote.Download(ctx, hash)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(r, domain.DefaultChunkSize+1))
		closeErr := r.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if int64(len(data)) > domain.DefaultChunkSize || !objectstore.Verify(data, hash) {
			return fmt.Errorf("downloaded chunk hash mismatch: %s", hash)
		}
		if err := e.Objects.Put(ctx, objectstore.Chunk{Hash: hash, Size: int64(len(data)), Data: data}); err != nil {
			return err
		}
	}
	dirty, err := e.localPathDirty(ctx, local)
	if err != nil {
		return err
	}
	if dirty {
		if err := e.preserveConflict(path, target, op.Revision); err != nil {
			return err
		}
	}
	if err := e.Objects.RestoreFile(ctx, m, target); err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if err := e.Index.UpsertFile(ctx, store.FileRecord{FileID: op.PathID, NormalizedPath: path, FileType: "regular", Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}); err != nil {
		return err
	}
	updated, err := e.localPathState(ctx, path)
	if err != nil {
		return err
	}
	newVersion, err := e.Index.CreateFileVersion(ctx, domain.FileID(updated.FileID), domain.FileVersionID(updated.VersionID), mid, false)
	if err != nil {
		return err
	}
	// Record an applied remote operation so the subsequent local scan does not
	// submit the same manifest as a new operation from this device.
	return e.Index.SaveSyncOperation(ctx, store.SyncOperation{
		OperationID: op.OperationID, SpaceID: e.SpaceID, NormalizedPath: path,
		ManifestID: op.FileManifestID, OperationType: "update", ParentRevision: op.ParentRevision,
		FileVersionID: string(newVersion), State: op.State, Revision: op.Revision,
	})
}

type localPathState struct {
	Path, FileID, VersionID, ManifestID string
	Tombstone                           bool
	Found                               bool
}

func (e *Engine) localPathState(ctx context.Context, path string) (localPathState, error) {
	var state localPathState
	var tombstone int
	err := e.Index.DB.QueryRowContext(ctx, `
SELECT f.normalized_path, f.file_id, COALESCE(f.current_version_id,''), COALESCE(v.manifest_id,''), f.tombstone
FROM files f
LEFT JOIN file_versions v ON v.version_id=f.current_version_id
WHERE f.normalized_path=?`, path).Scan(&state.Path, &state.FileID, &state.VersionID, &state.ManifestID, &tombstone)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.Tombstone = tombstone != 0
	state.Found = true
	return state, nil
}

func (e *Engine) localPathDirty(ctx context.Context, state localPathState) (bool, error) {
	if !state.Found {
		return false, nil
	}
	operationType := "update"
	if state.Tombstone {
		operationType = "delete"
	}
	if state.ManifestID == "" {
		return state.Tombstone, nil
	}
	if state.VersionID != "" {
		op, err := e.Index.FindSyncOperationByVersion(ctx, e.SpaceID, state.Path, state.ManifestID, operationType, state.VersionID)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return op.State != "committed", nil
	}
	op, err := e.Index.FindSyncOperationByType(ctx, e.SpaceID, state.Path, state.ManifestID, operationType)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return op.State != "committed", nil
}

func (e *Engine) preserveConflict(path, target string, revision int64) error {
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("cannot preserve directory conflict for %q", path)
	}
	base := strings.TrimSuffix(path, filepath.Ext(path))
	ext := filepath.Ext(path)
	device := sanitizeConflictPart(e.DeviceID)
	for attempt := 0; ; attempt++ {
		suffix := ".conflict-" + device + "-r" + strconv.FormatInt(revision, 10)
		if attempt > 0 {
			suffix += "-" + strconv.Itoa(attempt)
		}
		candidate := base + suffix + ext
		conflictTarget, err := e.safePath(candidate)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(conflictTarget); errors.Is(err, os.ErrNotExist) {
			return os.Rename(target, conflictTarget)
		} else if err != nil {
			return err
		}
	}
}

func sanitizeConflictPart(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "device"
	}
	return b.String()
}

func (e *Engine) pushLocal(ctx context.Context, parent int64) error {
	entries, err := (Scanner{FS: filesystem.OSFileSystem{}, Root: e.Root}).ScanContext(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Info.IsDir || !entry.Info.Mode.IsRegular() {
			continue
		}
		path, err := e.safePath(entry.RelativePath)
		if err != nil {
			return err
		}
		m, err := objectstore.BuildFile(ctx, path, e.Objects, e.ChunkSize)
		if err != nil {
			return err
		}
		mid, err := e.Objects.PutManifest(ctx, m)
		if err != nil {
			return err
		}
		manifestBytes, err := objectstore.MarshalManifest(m)
		if err != nil {
			return err
		}
		if err := e.uploadMissing(ctx, []objectstore.Chunk{{Hash: domain.ChunkHash(mid), Size: int64(len(manifestBytes)), Data: manifestBytes}}, m); err != nil {
			return err
		}
		localState, err := e.localPathState(ctx, entry.RelativePath)
		if err != nil {
			return err
		}
		fileVersionID := localState.VersionID
		opID := operationIDForVersion(entry.RelativePath, mid, fileVersionID)
		outbox, findErr := e.Index.FindSyncOperationByVersion(ctx, e.SpaceID, entry.RelativePath, string(mid), "update", fileVersionID)
		if errors.Is(findErr, sql.ErrNoRows) && fileVersionID != "" {
			outbox, findErr = e.Index.FindLegacyUpdateForVersion(ctx, e.SpaceID, entry.RelativePath, string(mid), fileVersionID)
		}
		if findErr == nil && outbox.State == "committed" {
			if outbox.Revision > parent {
				parent = outbox.Revision
			}
			continue
		}
		if findErr != nil && !errors.Is(findErr, sql.ErrNoRows) {
			return findErr
		}
		if findErr == nil {
			opID, parent = outbox.OperationID, outbox.ParentRevision
		}
		prepared, err := e.Remote.PrepareOperation(ctx, protocol.OperationPrepareRequest{OperationID: opID, ParentRevision: parent, PathID: encodePathID(entry.RelativePath), OperationType: "update", FileManifestID: string(mid)})
		if err != nil {
			return err
		}
		if err := e.Index.SaveSyncOperation(ctx, store.SyncOperation{OperationID: prepared.OperationID, SpaceID: e.SpaceID, NormalizedPath: entry.RelativePath, ManifestID: string(mid), FileVersionID: fileVersionID, ParentRevision: parent, State: prepared.State, Revision: prepared.Revision}); err != nil {
			return err
		}
		if prepared.State == "committed" {
			if err := e.recordLocalManifest(ctx, entry, m, mid); err != nil {
				return err
			}
			parent = prepared.Revision
			continue
		}
		committed, err := e.Remote.CommitOperation(ctx, protocol.OperationCommitRequest{OperationID: prepared.OperationID})
		if err != nil {
			return err
		}
		if committed.Revision > parent {
			parent = committed.Revision
		}
		if err := e.Index.SaveSyncOperation(ctx, store.SyncOperation{OperationID: committed.OperationID, SpaceID: e.SpaceID, NormalizedPath: entry.RelativePath, ManifestID: string(mid), FileVersionID: fileVersionID, ParentRevision: parent, State: committed.State, Revision: committed.Revision}); err != nil {
			return err
		}
		if err := e.recordLocalManifest(ctx, entry, m, mid); err != nil {
			return err
		}
	}
	pendingRenames, err := e.Index.ListPendingRenameDeletes(ctx)
	if err != nil {
		return err
	}
	for _, pending := range pendingRenames {
		parent, err = e.pushDelete(ctx, parent, pending.Path, pending.VersionID, pending.ManifestID, pending.DeleteID)
		if err != nil {
			return err
		}
	}
	tombstones, err := e.Index.ListTombstones(ctx)
	if err != nil {
		return err
	}
	for _, tombstone := range tombstones {
		parent, err = e.pushDelete(ctx, parent, tombstone.Path, tombstone.VersionID, tombstone.ManifestID, "")
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) pushDelete(ctx context.Context, parent int64, path, versionID, manifestID, pendingDeleteID string) (int64, error) {
	deleteKey := "delete:" + versionID
	if versionID == "" {
		deleteKey += manifestID
	}
	opID := operationIDForVersion(path, domain.ManifestID(deleteKey), versionID)
	legacyOpID := operationID(path, domain.ManifestID(deleteKey))
	outbox, findErr := e.Index.FindSyncOperationByVersion(ctx, e.SpaceID, path, deleteKey, "delete", versionID)
	if findErr == nil && outbox.State == "committed" {
		if outbox.Revision > parent {
			parent = outbox.Revision
		}
		if pendingDeleteID != "" {
			if err := e.Index.RemovePendingRenameDelete(ctx, pendingDeleteID); err != nil {
				return parent, err
			}
		}
		return parent, nil
	}
	if findErr != nil && !errors.Is(findErr, sql.ErrNoRows) {
		return parent, findErr
	}
	// Older clients stored the source manifest as the delete outbox key. The
	// operation ID is unchanged, so recover an already committed delete by ID
	// before attempting a prepare with a newer parent revision.
	if errors.Is(findErr, sql.ErrNoRows) {
		legacy, legacyErr := e.Index.FindSyncOperationByID(ctx, legacyOpID)
		if legacyErr == nil {
			if legacy.State == "committed" {
				if legacy.Revision > parent {
					parent = legacy.Revision
				}
				if pendingDeleteID != "" {
					if err := e.Index.RemovePendingRenameDelete(ctx, pendingDeleteID); err != nil {
						return parent, err
					}
				}
				return parent, nil
			}
			opID, parent = legacy.OperationID, legacy.ParentRevision
		} else if !errors.Is(legacyErr, sql.ErrNoRows) {
			return parent, legacyErr
		}
	}
	if findErr == nil {
		opID, parent = outbox.OperationID, outbox.ParentRevision
	}
	prepared, err := e.Remote.PrepareOperation(ctx, protocol.OperationPrepareRequest{OperationID: opID, ParentRevision: parent, PathID: encodePathID(path), OperationType: "delete"})
	if err != nil {
		return parent, err
	}
	if err := e.Index.SaveSyncOperation(ctx, store.SyncOperation{OperationID: prepared.OperationID, SpaceID: e.SpaceID, NormalizedPath: path, ManifestID: deleteKey, OperationType: "delete", FileVersionID: versionID, ParentRevision: parent, State: prepared.State, Revision: prepared.Revision}); err != nil {
		return parent, err
	}
	if prepared.State == "committed" {
		if prepared.Revision > parent {
			parent = prepared.Revision
		}
	} else {
		committed, err := e.Remote.CommitOperation(ctx, protocol.OperationCommitRequest{OperationID: prepared.OperationID})
		if err != nil {
			return parent, err
		}
		if committed.Revision > parent {
			parent = committed.Revision
		}
		if err := e.Index.SaveSyncOperation(ctx, store.SyncOperation{OperationID: committed.OperationID, SpaceID: e.SpaceID, NormalizedPath: path, ManifestID: deleteKey, OperationType: "delete", FileVersionID: versionID, ParentRevision: parent, State: committed.State, Revision: committed.Revision}); err != nil {
			return parent, err
		}
	}
	if pendingDeleteID != "" {
		if err := e.Index.RemovePendingRenameDelete(ctx, pendingDeleteID); err != nil {
			return parent, err
		}
	}
	return parent, nil
}

func (e *Engine) recordLocalManifest(ctx context.Context, entry Entry, manifest objectstore.Manifest, manifestID domain.ManifestID) error {
	if _, err := e.Index.SaveManifest(ctx, manifest); err != nil {
		return err
	}
	state, err := e.localPathState(ctx, entry.RelativePath)
	if err != nil {
		return err
	}
	if state.Found && !state.Tombstone && state.ManifestID == string(manifestID) {
		return nil
	}
	fileID := entry.Info.PlatformID
	if fileID == "" {
		fileID = "path:" + entry.RelativePath
	}
	if err := e.Index.UpsertFile(ctx, store.FileRecord{FileID: fileID, NormalizedPath: entry.RelativePath, FileType: "regular", Size: entry.Info.Size, MTimeNS: entry.Info.ModTime.UnixNano(), PlatformID: entry.Info.PlatformID}); err != nil {
		return err
	}
	state, err = e.localPathState(ctx, entry.RelativePath)
	if err != nil {
		return err
	}
	_, err = e.Index.CreateFileVersion(ctx, domain.FileID(state.FileID), domain.FileVersionID(state.VersionID), manifestID, false)
	return err
}

func (e *Engine) uploadMissing(ctx context.Context, manifestObject []objectstore.Chunk, m objectstore.Manifest) error {
	hashes := make([]domain.ChunkHash, 0, len(m.Chunks)+1)
	hashes = append(hashes, manifestObject[0].Hash)
	hashes = append(hashes, m.Chunks...)
	present := make(map[domain.ChunkHash]bool, len(hashes))
	for start := 0; start < len(hashes); start += 1000 {
		end := start + 1000
		if end > len(hashes) {
			end = len(hashes)
		}
		part, err := e.Remote.Exists(ctx, hashes[start:end])
		if err != nil {
			return err
		}
		for hash, ok := range part {
			present[hash] = ok
		}
	}
	for _, obj := range manifestObject {
		if !present[obj.Hash] {
			if err := e.Remote.Upload(ctx, obj.Hash, bytes.NewReader(obj.Data)); err != nil {
				return err
			}
		}
	}
	for _, hash := range m.Chunks {
		if present[hash] {
			continue
		}
		data, err := e.Objects.Read(ctx, hash)
		if err != nil {
			return err
		}
		if err := e.Remote.Upload(ctx, hash, bytes.NewReader(data)); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) safePath(rel string) (string, error) {
	normalized, err := filesystem.NormalizeRelativePath(rel)
	if err != nil {
		return "", err
	}
	target := filepath.Join(e.Root, filepath.FromSlash(normalized))
	root := filepath.Clean(e.Root)
	relToRoot, err := filepath.Rel(root, target)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", domain.ErrInvalidPath
	}
	// A lexical check is insufficient when a directory inside the sync root is
	// a symlink. Reject symlink components before any read/write operation.
	current := root
	for _, part := range strings.Split(relToRoot, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", domain.ErrInvalidPath
		}
		if statErr != nil && os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
	}
	return target, nil
}

func encodePathID(path string) string { return "p-" + hex.EncodeToString([]byte(path)) }
func decodePathID(id string) (string, error) {
	if !strings.HasPrefix(id, "p-") {
		return filesystem.NormalizeRelativePath(id)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(id, "p-"))
	if err != nil {
		return "", err
	}
	return filesystem.NormalizeRelativePath(string(b))
}
func operationID(path string, id domain.ManifestID) string {
	return "op-" + string(objectstore.Hash([]byte(path+"\x00"+string(id))))
}

func operationIDForVersion(path string, id domain.ManifestID, fileVersionID string) string {
	if fileVersionID == "" {
		return operationID(path, id)
	}
	return "op-" + string(objectstore.Hash([]byte(path+"\x00"+fileVersionID+"\x00"+string(id))))
}
