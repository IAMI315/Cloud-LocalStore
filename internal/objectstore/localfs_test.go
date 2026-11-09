package objectstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
