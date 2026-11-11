package sync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
	"github.com/IAMI315/Cloud-LocalStore/internal/store"
	_ "modernc.org/sqlite"
)

type fakeRemote struct {
	objects      map[domain.ChunkHash][]byte
	ops          []protocol.Operation
	current      int64
	ack          int64
	uploadCalls  int
	failUploadAt int
}

func (f *fakeRemote) Exists(_ context.Context, hs []domain.ChunkHash) (map[domain.ChunkHash]bool, error) {
	out := map[domain.ChunkHash]bool{}
	for _, h := range hs {
		_, out[h] = f.objects[h]
	}
	return out, nil
}
func (f *fakeRemote) Upload(_ context.Context, h domain.ChunkHash, r io.Reader) error {
	f.uploadCalls++
	if f.failUploadAt > 0 && f.uploadCalls == f.failUploadAt {
		return errors.New("injected upload interruption")
	}
	b, e := io.ReadAll(r)
	if e == nil {
		f.objects[h] = b
	}
	return e
}

func TestLargeUploadResumesUsingAlreadyUploadedChunks(t *testing.T) {
	remote := &fakeRemote{objects: map[domain.ChunkHash][]byte{}, failUploadAt: 3}
	root := t.TempDir()
	data := make([]byte, int(domain.DefaultChunkSize*2+257))
	for i := range data {
		data[i] = byte((int64(i)/domain.DefaultChunkSize*71 + int64(i)%251) % 256)
	}
	if err := os.WriteFile(filepath.Join(root, "large.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, root, remote)
	if err := e.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "injected upload interruption") {
		t.Fatalf("first sync error=%v, want injected interruption", err)
	}
	if got := len(remote.objects); got != 2 {
		t.Fatalf("uploaded objects after interruption=%d, want manifest plus one chunk", got)
	}
	remote.failUploadAt = 0
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remote.uploadCalls != 5 {
		t.Fatalf("upload calls=%d, want 5 (manifest, one chunk, failed chunk, then two missing chunks)", remote.uploadCalls)
	}
	if len(remote.ops) != 1 || remote.ops[0].State != "committed" {
		t.Fatalf("remote operations=%+v, want one committed update", remote.ops)
	}
}

func TestEnginePublishesRenameAsNewPathAndOldPathDelete(t *testing.T) {
	remote := &fakeRemote{objects: map[domain.ChunkHash][]byte{}}
	root := t.TempDir()
	oldPath := filepath.Join(root, "old.txt")
	if err := os.WriteFile(oldPath, []byte("rename me"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, root, remote)
	ctx := context.Background()
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	rootB := t.TempDir()
	b := testEngineWithDevice(t, rootB, remote, "device-b")
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var fileID string
	if err := e.Index.DB.QueryRowContext(ctx, `SELECT file_id FROM files WHERE normalized_path=?`, "old.txt").Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Index.DB.ExecContext(ctx, `UPDATE files SET platform_id=? WHERE normalized_path=?`, "stable-file-identity", "old.txt"); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(root, "renamed.txt")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	entries, err := (Scanner{FS: filesystem.OSFileSystem{}, Root: root}).ScanContext(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("scan renamed file=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if err := e.Index.ReplaceFileIndex(ctx, []store.FileRecord{{FileID: fileID, NormalizedPath: entry.RelativePath, FileType: "regular", Size: entry.Info.Size, MTimeNS: entry.Info.ModTime.UnixNano(), PlatformID: "stable-file-identity"}}); err != nil {
		t.Fatal(err)
	}
	pending, err := e.Index.ListPendingRenameDeletes(ctx)
	if err != nil || len(pending) != 1 || pending[0].Path != "old.txt" {
		t.Fatalf("pending rename delete=%+v err=%v", pending, err)
	}
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old path still exists on second device, stat error=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(rootB, "renamed.txt"))
	if err != nil || string(data) != "rename me" {
		t.Fatalf("renamed file data=%q err=%v", data, err)
	}
	latest := map[string]string{}
	for _, op := range remote.ops {
		if op.State != "committed" {
			continue
		}
		path, err := decodePathID(op.PathID)
		if err != nil {
			t.Fatal(err)
		}
		latest[path] = op.OperationType
	}
	if latest["old.txt"] != "delete" || latest["renamed.txt"] != "update" {
		t.Fatalf("remote latest operations=%v", latest)
	}
	pending, err = e.Index.ListPendingRenameDeletes(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("rename queue not drained: %+v err=%v", pending, err)
	}
}
func (f *fakeRemote) Download(_ context.Context, h domain.ChunkHash) (io.ReadCloser, error) {
	b, ok := f.objects[h]
	if !ok {
		return nil, io.EOF
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *fakeRemote) PrepareOperation(_ context.Context, r protocol.OperationPrepareRequest) (protocol.Operation, error) {
	for _, o := range f.ops {
		if o.OperationID == r.OperationID {
			return o, nil
		}
	}
	o := protocol.Operation{OperationID: r.OperationID, PathID: r.PathID, OperationType: r.OperationType, FileManifestID: r.FileManifestID, State: "prepared"}
	f.ops = append(f.ops, o)
	return o, nil
}
func (f *fakeRemote) CommitOperation(_ context.Context, r protocol.OperationCommitRequest) (protocol.Operation, error) {
	for i := range f.ops {
		if f.ops[i].OperationID == r.OperationID {
			f.current++
			f.ops[i].Revision = f.current
			f.ops[i].State = "committed"
			return f.ops[i], nil
		}
	}
	return protocol.Operation{}, io.EOF
}
func (f *fakeRemote) ListOperations(_ context.Context, after int64) (protocol.OperationListResponse, error) {
	out := protocol.OperationListResponse{CurrentRevision: f.current}
	for _, o := range f.ops {
		if o.State == "committed" && o.Revision > after {
			out.Operations = append(out.Operations, o)
		}
	}
	return out, nil
}
func (f *fakeRemote) Ack(_ context.Context, r protocol.AckRequest) (protocol.AckResponse, error) {
	f.ack = r.AckRevision
	return protocol.AckResponse{LastSeenRevision: r.LastSeenRevision, LastAppliedRevision: r.LastAppliedRevision, LastAckRevision: r.AckRevision}, nil
}

func TestDecodePathIDRejectsTraversal(t *testing.T) {
	if _, err := decodePathID(encodePathID("../outside")); err == nil {
		t.Fatal("expected traversal rejection")
	}
}
func TestOperationIDStable(t *testing.T) {
	if operationID("a", "b") != operationID("a", "b") {
		t.Fatal("operation id must be stable")
	}
}

func TestDeleteOperationIDChangesWithLocalVersion(t *testing.T) {
	first := operationID("file.txt", "delete:v-first")
	second := operationID("file.txt", "delete:v-second")
	if first == second {
		t.Fatal("delete operations for different local versions must not share an operation id")
	}
}

func testEngine(t *testing.T, root string, remote *fakeRemote) *Engine {
	return testEngineWithDevice(t, root, remote, "device")
}

func testEngineWithDevice(t *testing.T, root string, remote *fakeRemote, deviceID string) *Engine {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "state.db")+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	index, err := store.OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objectstore.NewLocalFS(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(root, "space", deviceID, objects, index, remote)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEngineUploadsAndAppliesRemoteFiles(t *testing.T) {
	remote := &fakeRemote{objects: map[domain.ChunkHash][]byte{}}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "local.txt"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, root, remote)
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.ops) != 1 || remote.ops[0].State != "committed" {
		t.Fatalf("uploaded ops=%+v", remote.ops)
	}

	chunk := objectstore.Hash([]byte("remote"))
	m := objectstore.Manifest{Size: 6, ChunkSize: domain.DefaultChunkSize, Chunks: []domain.ChunkHash{chunk}, HashAlgorithm: "BLAKE3-256", FormatVersion: objectstore.ManifestFormatVersion}
	manifestBytes, err := objectstore.MarshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	mid := objectstore.Hash(manifestBytes)
	remote.objects[chunk] = []byte("remote")
	remote.objects[mid] = manifestBytes
	remote.current = 1
	remote.ops = []protocol.Operation{{OperationID: "remote-op", PathID: encodePathID("remote.txt"), OperationType: "update", FileManifestID: string(mid), State: "committed", Revision: 1}}
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "remote.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "remote" {
		t.Fatalf("remote content=%q", got)
	}
	if remote.ack != 1 {
		t.Fatalf("ack=%d", remote.ack)
	}
}

func TestEnginePreservesDirtyLocalFileOnRemoteUpdate(t *testing.T) {
	remote := &fakeRemote{objects: map[domain.ChunkHash][]byte{}}
	root := t.TempDir()
	localData := []byte("local")
	if err := os.WriteFile(filepath.Join(root, "report.txt"), localData, 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, root, remote)
	ctx := context.Background()
	localChunk := objectstore.Chunk{Hash: objectstore.Hash(localData), Size: int64(len(localData)), Data: localData}
	if err := e.Objects.Put(ctx, localChunk); err != nil {
		t.Fatal(err)
	}
	localManifest := objectstore.Manifest{Size: int64(len(localData)), ChunkSize: domain.DefaultChunkSize, Chunks: []domain.ChunkHash{localChunk.Hash}, HashAlgorithm: "BLAKE3-256", FormatVersion: objectstore.ManifestFormatVersion}
	localManifestID, err := e.Objects.PutManifest(ctx, localManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Index.SaveManifest(ctx, localManifest); err != nil {
		t.Fatal(err)
	}
	if err := e.Index.ReplaceFileIndex(ctx, []store.FileRecord{{FileID: "local-file", NormalizedPath: "report.txt", FileType: "regular", Size: int64(len(localData))}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Index.CreateFileVersion(ctx, "local-file", "", localManifestID, false); err != nil {
		t.Fatal(err)
	}

	remoteData := []byte("remote")
	remoteChunk := objectstore.Chunk{Hash: objectstore.Hash(remoteData), Size: int64(len(remoteData)), Data: remoteData}
	remote.objects[remoteChunk.Hash] = remoteData
	remoteManifest := objectstore.Manifest{Size: int64(len(remoteData)), ChunkSize: domain.DefaultChunkSize, Chunks: []domain.ChunkHash{remoteChunk.Hash}, HashAlgorithm: "BLAKE3-256", FormatVersion: objectstore.ManifestFormatVersion}
	remoteManifestBytes, err := objectstore.MarshalManifest(remoteManifest)
	if err != nil {
		t.Fatal(err)
	}
	remoteManifestID := objectstore.Hash(remoteManifestBytes)
	remote.objects[remoteManifestID] = remoteManifestBytes
	remote.ops = []protocol.Operation{{OperationID: "remote-update", OriginDeviceID: "other-device", PathID: encodePathID("report.txt"), OperationType: "update", FileManifestID: string(remoteManifestID), State: "committed", Revision: 1}}
	remote.current = 1

	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "remote" {
		t.Fatalf("primary file=%q, want remote", got)
	}
	conflict, err := filepath.Glob(filepath.Join(root, "report.conflict-device-r1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(conflict) != 1 {
		t.Fatalf("conflict copy count=%d, want 1", len(conflict))
	}
	got, err = os.ReadFile(conflict[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "local" {
		t.Fatalf("conflict file=%q, want local", got)
	}
}

func TestEngineReappliesOwnRemoteUpdateWhenLocalFileIsMissing(t *testing.T) {
	remote := &fakeRemote{objects: map[domain.ChunkHash][]byte{}}
	root := t.TempDir()
	e := testEngine(t, root, remote)
	ctx := context.Background()
	data := []byte("recover me")
	chunk := objectstore.Chunk{Hash: objectstore.Hash(data), Size: int64(len(data)), Data: data}
	remote.objects[chunk.Hash] = data
	m := objectstore.Manifest{Size: int64(len(data)), ChunkSize: domain.DefaultChunkSize, Chunks: []domain.ChunkHash{chunk.Hash}, HashAlgorithm: "BLAKE3-256", FormatVersion: objectstore.ManifestFormatVersion}
	manifestBytes, err := objectstore.MarshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	manifestID := objectstore.Hash(manifestBytes)
	remote.objects[manifestID] = manifestBytes
	remote.ops = []protocol.Operation{{OperationID: "own-update", OriginDeviceID: "device", PathID: encodePathID("recovered.txt"), OperationType: "update", FileManifestID: string(manifestID), State: "committed", Revision: 1}}
	remote.current = 1

	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "recovered.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("recovered content=%q, want %q", got, data)
	}
}

func TestEnginePublishesRecreatedPathWithSameContent(t *testing.T) {
	remote := &fakeRemote{objects: map[domain.ChunkHash][]byte{}}
	root := t.TempDir()
	path := filepath.Join(root, "same.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, root, remote)
	ctx := context.Background()
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	advanceCursor := func() {
		state, err := e.Index.GetSyncState(ctx, e.SpaceID)
		if err != nil {
			t.Fatal(err)
		}
		state.LastSeenRevision = remote.current
		state.LastAppliedRevision = remote.current
		state.LastAckRevision = remote.current
		if err := e.Index.SaveSyncState(ctx, state); err != nil {
			t.Fatal(err)
		}
	}
	advanceCursor()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := e.Index.TombstoneFile(ctx, "same.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	advanceCursor()

	// Recreate the same path with the same bytes. This is the case that used to
	// reuse the old update id after a sync-directory change.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Index.ReplaceFileIndex(ctx, []store.FileRecord{{FileID: "path:same.txt", NormalizedPath: "same.txt", FileType: "regular", Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}}); err != nil {
		t.Fatal(err)
	}
	manifest, err := objectstore.BuildFile(ctx, path, e.Objects, e.ChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	manifestID, err := e.Index.SaveManifest(ctx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Index.CreateFileVersion(ctx, "path:same.txt", "", manifestID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	var updates, deletes int
	for _, op := range remote.ops {
		if op.State != "committed" {
			continue
		}
		path, err := decodePathID(op.PathID)
		if err != nil {
			t.Fatal(err)
		}
		if path != "same.txt" {
			continue
		}
		switch op.OperationType {
		case "update":
			updates++
		case "delete":
			deletes++
		}
	}
	if updates != 2 || deletes != 1 {
		t.Fatalf("committed operations for recreated path: updates=%d deletes=%d all=%+v", updates, deletes, remote.ops)
	}
}

func TestTwoClientsCatchUpAfterOneWasOffline(t *testing.T) {
	remote := &fakeRemote{objects: map[domain.ChunkHash][]byte{}}
	rootA, rootB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(rootA, "shared.txt"), []byte("version-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := testEngineWithDevice(t, rootA, remote, "device-a")
	b := testEngineWithDevice(t, rootB, remote, "device-b")
	ctx := context.Background()
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	remote.ops[0].OriginDeviceID = "device-a"
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rootB, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version-1" {
		t.Fatalf("initial catch-up content=%q", got)
	}
	if err := os.WriteFile(filepath.Join(rootA, "shared.txt"), []byte("version-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(rootB, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version-2" {
		t.Fatalf("offline catch-up content=%q", got)
	}
}
