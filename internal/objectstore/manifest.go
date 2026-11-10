package objectstore

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
)

const ManifestFormatVersion = 1

type Manifest struct {
	Size          int64              `json:"size"`
	ChunkSize     int64              `json:"chunk_size"`
	Chunks        []domain.ChunkHash `json:"chunks"`
	HashAlgorithm string             `json:"hash_algorithm"`
	FormatVersion int                `json:"format_version"`
}

func BuildManifest(r io.Reader, chunkSize int64, put func(Chunk) error) (Manifest, error) {
	if chunkSize <= 0 {
		chunkSize = domain.DefaultChunkSize
	}
	m := Manifest{ChunkSize: chunkSize, HashAlgorithm: "BLAKE3-256", FormatVersion: ManifestFormatVersion}
	err := Split(r, chunkSize, func(c Chunk) error { m.Size += c.Size; m.Chunks = append(m.Chunks, c.Hash); return put(c) })
	return m, err
}

func (m Manifest) Validate() error {
	if m.FormatVersion != ManifestFormatVersion {
		return fmt.Errorf("unsupported manifest format %d", m.FormatVersion)
	}
	if m.HashAlgorithm != "BLAKE3-256" {
		return fmt.Errorf("unsupported hash algorithm %q", m.HashAlgorithm)
	}
	if m.ChunkSize <= 0 || m.Size < 0 {
		return fmt.Errorf("invalid manifest dimensions")
	}
	if len(m.Chunks) == 0 && m.Size != 0 {
		return fmt.Errorf("non-empty manifest has no chunks")
	}
	for i, hash := range m.Chunks {
		if len(hash) != 64 {
			return fmt.Errorf("invalid chunk hash length at index %d", i)
		}
		if _, err := hex.DecodeString(string(hash)); err != nil {
			return fmt.Errorf("invalid chunk hash at index %d: %w", i, err)
		}
	}
	return nil
}

func MarshalManifest(m Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}
func UnmarshalManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
