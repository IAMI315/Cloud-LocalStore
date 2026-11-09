package objectstore

import (
	"crypto/subtle"
	"fmt"
	"io"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/zeebo/blake3"
)

type Chunk struct {
	Hash domain.ChunkHash
	Size int64
	Data []byte
}

func Hash(data []byte) domain.ChunkHash {
	sum := blake3.Sum256(data)
	return domain.ChunkHash(fmt.Sprintf("%x", sum[:]))
}

func Verify(data []byte, expected domain.ChunkHash) bool {
	actual := Hash(data)
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func Split(r io.Reader, chunkSize int64, fn func(Chunk) error) error {
	if chunkSize <= 0 { chunkSize = domain.DefaultChunkSize }
	buf := make([]byte, chunkSize)
	for {
		n, err := io.ReadFull(r, buf)
		if err == io.EOF { return nil }
		if err != nil && err != io.ErrUnexpectedEOF { return err }
		data := append([]byte(nil), buf[:n]...)
		if err := fn(Chunk{Hash: Hash(data), Size: int64(n), Data: data}); err != nil { return err }
		if err == io.ErrUnexpectedEOF { return nil }
	}
}
