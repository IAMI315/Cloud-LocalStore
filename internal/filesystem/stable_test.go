package filesystem

import (
	"errors"
	"testing"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
)

type changingFileSystem struct {
	FileSystem
	statCalls int
}

func (f *changingFileSystem) Stat(path string) (FileInfo, error) {
	f.statCalls++
	return FileInfo{Path: path, Size: int64(f.statCalls), ModTime: time.Unix(int64(f.statCalls), 0)}, nil
}

func TestStableReaderReturnsUnstableSentinel(t *testing.T) {
	reader := StableReader{
		FS:          &changingFileSystem{},
		PollDelay:   time.Millisecond,
		MaxAttempts: 2,
	}

	_, err := reader.Check("changing.txt")
	if !errors.Is(err, domain.ErrUnstableFile) {
		t.Fatalf("expected ErrUnstableFile, got %v", err)
	}
}

var _ FileSystem = (*changingFileSystem)(nil)
