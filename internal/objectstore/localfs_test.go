package objectstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
)

func TestLocalFSPutReadVerifyAndDeduplicate(t *testing.T) {
	s, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := Chunk{Data: []byte("hello")}
	c.Hash = Hash(c.Data)
	c.Size = int64(len(c.Data))
	if err := s.Put(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(context.Background(), c.Hash); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(context.Background(), c.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestLocalFSRejectsCorruptChunk(t *testing.T) {
	s, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), Chunk{Hash: "bad", Size: 1, Data: []byte("x")}); err == nil {
		t.Fatal("expected hash mismatch")
	}
}

func TestLocalFSRejectsUnsafeHash(t *testing.T) {
	s, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(context.Background(), "../escape"); err == nil {
		t.Fatal("expected invalid hash error")
	}
}

func TestLocalFSCleansInterruptedUploads(t *testing.T) {
	root := t.TempDir()
	s, err := NewLocalFS(root)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, "ab", ".upload-interrupted")
	if err := os.MkdirAll(filepath.Dir(tmp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupTemp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temporary upload still exists: %v", err)
	}
}

func TestBuildFileStoresFixedChunks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.bin")
	if err := os.WriteFile(path, []byte("abcdefghij"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewLocalFS(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := BuildFile(context.Background(), path, store, 4)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != 10 || len(m.Chunks) != 3 {
		t.Fatalf("manifest=%#v", m)
	}
	for _, hash := range m.Chunks {
		if err := store.Verify(context.Background(), hash); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestoreFileAtomicallyRebuildsTarget(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalFS(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildFile(context.Background(), filepath.Join(root, "input.bin"), store, 4)
	if err == nil {
		t.Fatal("expected missing input error")
	}
	data := []byte("abcdefghij")
	input := filepath.Join(root, "input.bin")
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err = BuildFile(context.Background(), input, store, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "restored", "file.bin")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreFile(context.Background(), manifest, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("restored %q", got)
	}
}

func TestRestoreFileLeavesTargetWhenChunkMissing(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalFS(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("abcdefgh")
	input := filepath.Join(root, "input.bin")
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildFile(context.Background(), input, store, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.path(manifest.Chunks[1])); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.bin")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreFile(context.Background(), manifest, target); err == nil {
		t.Fatal("expected missing chunk error")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("target changed after failed restore: %q", got)
	}
}

func TestLocalFSStoresAndReadsManifest(t *testing.T) {
	s, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Size: 5, ChunkSize: 4, Chunks: []domain.ChunkHash{Hash([]byte("hell")), Hash([]byte("o"))}, HashAlgorithm: "BLAKE3-256", FormatVersion: ManifestFormatVersion}
	id, err := s.PutManifest(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadManifest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != m.Size || len(got.Chunks) != len(m.Chunks) {
		t.Fatalf("manifest mismatch: %#v", got)
	}
}
