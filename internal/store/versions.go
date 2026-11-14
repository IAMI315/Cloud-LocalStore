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
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunks(chunk_hash,size) VALUES(?,?) ON CONFLICT(chunk_hash) DO UPDATE SET size=excluded.size, verified_at=datetime('now','localtime')`, chunk.Hash, chunk.Size); err != nil {
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
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM files WHERE file_id=?`, fileID).Scan(&exists); err != nil {
		return "", fmt.Errorf("check file: %w", err)
	}
	if exists == 0 {
		return "", fmt.Errorf("file not found: %s", fileID)
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM manifests WHERE manifest_id=?`, manifest).Scan(&exists); err != nil {
		return "", fmt.Errorf("check manifest: %w", err)
	}
	if exists == 0 {
		return "", fmt.Errorf("manifest not found: %s", manifest)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO file_versions(version_id,file_id,parent_version_id,manifest_id,is_conflict) VALUES(?,?,?,?,?)`, version, fileID, nullableID(parent), manifest, boolInt(conflict)); err != nil {
		return "", fmt.Errorf("create file version: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE files SET current_version_id=?, tombstone=0, updated_at=datetime('now','localtime') WHERE file_id=?`, version, fileID)
	if err != nil {
		return "", err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("file update affected %d rows", affected)
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

// RestoreFileVersionTo applies a historical version to target and then records
// the restoration as a new current version. The target is only replaced after
// every referenced chunk has been read and verified.
func (s *ClientStore) RestoreFileVersionTo(ctx context.Context, fileID domain.FileID, source domain.FileVersionID, target string, objects *objectstore.LocalFS) (domain.FileVersionID, error) {
	if objects == nil {
		return "", fmt.Errorf("object store is required")
	}
	var manifestID domain.ManifestID
	if err := s.DB.QueryRowContext(ctx, `SELECT manifest_id FROM file_versions WHERE file_id=? AND version_id=?`, fileID, source).Scan(&manifestID); err != nil {
		return "", err
	}
	manifest, err := objects.ReadManifest(ctx, manifestID)
	if err != nil {
		return "", fmt.Errorf("read manifest %s: %w", manifestID, err)
	}
	if err := objects.RestoreFile(ctx, manifest, target); err != nil {
		return "", fmt.Errorf("restore file: %w", err)
	}
	return s.CreateFileVersion(ctx, fileID, source, manifestID, false)
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
