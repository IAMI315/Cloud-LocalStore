package objectstore

import (
	"bytes"
	"testing"
)

func TestSplitUsesFixedChunksAndRoundTrips(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 10)
	var chunks []Chunk
	if err := Split(bytes.NewReader(data), 4, func(c Chunk) error { chunks = append(chunks, c); return nil }); err != nil { t.Fatal(err) }
	if len(chunks) != 3 || chunks[0].Size != 4 || chunks[2].Size != 2 { t.Fatalf("unexpected chunks: %#v", chunks) }
	var rebuilt []byte
	for _, c := range chunks { rebuilt = append(rebuilt, c.Data...); if !Verify(c.Data, c.Hash) { t.Fatalf("chunk %s did not verify", c.Hash) } }
	if !bytes.Equal(rebuilt, data) { t.Fatal("chunks did not round trip") }
}

func TestManifestRejectsUnsupportedFormat(t *testing.T) {
	m := Manifest{Size: 1, ChunkSize: 4, Chunks: []string{"x"}, HashAlgorithm: "BLAKE3-256", FormatVersion: 99}
	if err := m.Validate(); err == nil { t.Fatal("expected unsupported format error") }
}
