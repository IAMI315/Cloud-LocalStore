package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
)

func ManifestID(m objectstore.Manifest) (domain.ManifestID, error) {
	b, err := objectstore.MarshalManifest(m)
	if err != nil {
		return "", err
	}
	return domain.ManifestID(objectstore.Hash(b)), nil
}

func (s *ClientStore) SaveManifest(ctx context.Context, m objectstore.Manifest) (domain.ManifestID, error) {
	id, err := ManifestID(m)
	if err != nil {
		return "", err
	}
	if err := m.Validate(); err != nil {
		return "", err
	}
	chunks, err := json.Marshal(m.Chunks)
	if err != nil {
		return "", err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO manifests(manifest_id,size,chunk_size,chunk_count,chunk_hashes,hash_algorithm,format_version) VALUES(?,?,?,?,?,?,?) ON CONFLICT(manifest_id) DO NOTHING`, id, m.Size, m.ChunkSize, len(m.Chunks), string(chunks), m.HashAlgorithm, m.FormatVersion)
	if err != nil {
		return "", fmt.Errorf("save manifest: %w", err)
	}
	return id, nil
}

func (s *ClientStore) SaveChunks(ctx context.Context, chunks []objectstore.Chunk) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, chunk := range chunks {
		if !objectstore.Verify(chunk.Data, chunk.Hash) {
			return fmt.Errorf("chunk hash mismatch: %s", chunk.Hash)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunks(chunk_hash,size) VALUES(?,?) ON CONFLICT(chunk_hash) DO UPDATE SET size=excluded.size, verified_at=CURRENT_TIMESTAMP`, chunk.Hash, chunk.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *ClientStore) CreateFileVersion(ctx context.Context, fileID domain.FileID, parent domain.FileVersionID, manifest domain.ManifestID, conflict bool) (domain.FileVersionID, error) {
	version, err := newFileVersionID()
	if err != nil {
		return "", fmt.Errorf("generate file version id: %w", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin create file version: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO file_versions(version_id,file_id,parent_version_id,manifest_id,is_conflict) VALUES(?,?,?,?,?)`, version, fileID, nullableID(parent), manifest, boolInt(conflict)); err != nil {
		return "", fmt.Errorf("create file version: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE files SET current_version_id=?, tombstone=0, updated_at=CURRENT_TIMESTAMP WHERE file_id=?`, version, fileID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("commit file version: %w", err)
	}
	return version, nil
}

func newFileVersionID() (domain.FileVersionID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return domain.FileVersionID("v-" + hex.EncodeToString(raw[:])), nil
}

func (s *ClientStore) RestoreFileVersion(ctx context.Context, fileID domain.FileID, source domain.FileVersionID) (domain.FileVersionID, error) {
	var manifest domain.ManifestID
	if err := s.DB.QueryRowContext(ctx, `SELECT manifest_id FROM file_versions WHERE file_id=? AND version_id=?`, fileID, source).Scan(&manifest); err != nil {
		return "", err
	}
	return s.CreateFileVersion(ctx, fileID, source, manifest, false)
}

func nullableID(id domain.FileVersionID) any {
	if id == "" {
		return nil
	}
	return id
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
