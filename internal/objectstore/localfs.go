package objectstore

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
)

type LocalFS struct{ Root string }

func NewLocalFS(root string) (*LocalFS, error) {
	if root == "" {
		return nil, fmt.Errorf("object store root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &LocalFS{Root: root}, nil
}

func (s *LocalFS) path(hash domain.ChunkHash) string {
	h := string(hash)
	prefix := h
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	return filepath.Join(s.Root, prefix, h)
}

func (s *LocalFS) Has(_ context.Context, hash domain.ChunkHash) (bool, error) {
	if err := s.ValidateHash(hash); err != nil {
		return false, err
	}
	_, err := os.Stat(s.path(hash))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *LocalFS) Put(ctx context.Context, chunk Chunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ValidateHash(chunk.Hash); err != nil {
		return err
	}
	if !Verify(chunk.Data, chunk.Hash) {
		return fmt.Errorf("chunk hash mismatch: %s", chunk.Hash)
	}
	target := s.path(chunk.Hash)
	if ok, err := s.Has(ctx, chunk.Hash); err != nil {
		return err
	} else if ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(chunk.Data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpName, target); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func (s *LocalFS) Open(_ context.Context, hash domain.ChunkHash) (io.ReadCloser, error) {
	if err := s.ValidateHash(hash); err != nil {
		return nil, err
	}
	return os.Open(s.path(hash))
}

func (s *LocalFS) Read(ctx context.Context, hash domain.ChunkHash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := s.Open(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (s *LocalFS) Verify(ctx context.Context, hash domain.ChunkHash) error {
	data, err := s.Read(ctx, hash)
	if err != nil {
		return err
	}
	if !Verify(data, hash) {
		return fmt.Errorf("stored chunk hash mismatch: %s", hash)
	}
	return nil
}

func (s *LocalFS) Delete(_ context.Context, hash domain.ChunkHash) error {
	if err := s.ValidateHash(hash); err != nil {
		return err
	}
	return os.Remove(s.path(hash))
}

func (s *LocalFS) ValidateHash(hash domain.ChunkHash) error {
	h := strings.TrimSpace(string(hash))
	if len(h) != 64 {
		return fmt.Errorf("invalid chunk hash length")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return fmt.Errorf("invalid chunk hash: %w", err)
	}
	return nil
}

func BuildFile(ctx context.Context, path string, s *LocalFS, chunkSize int64) (Manifest, error) {
	if s == nil {
		return Manifest{}, fmt.Errorf("object store is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	return BuildManifest(f, chunkSize, func(chunk Chunk) error { return s.Put(ctx, chunk) })
}
