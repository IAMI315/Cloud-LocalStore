package filesystem

import (
	"fmt"
	"time"
)

type StableReader struct {
	FS          FileSystem
	PollDelay   time.Duration
	MaxAttempts int
}

func (r StableReader) Check(path string) (FileInfo, error) {
	if r.PollDelay <= 0 {
		r.PollDelay = 100 * time.Millisecond
	}
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 3
	}
	var previous FileInfo
	for attempt := 0; attempt < r.MaxAttempts; attempt++ {
		before, err := r.FS.Stat(path)
		if err != nil {
			return FileInfo{}, err
		}
		if before.IsDir {
			return before, nil
		}
		if attempt > 0 && before.Size == previous.Size && before.ModTime.Equal(previous.ModTime) {
			return before, nil
		}
		previous = before
		time.Sleep(r.PollDelay)
	}
	return FileInfo{}, fmt.Errorf("%w: %s", ErrUnstableFile, path)
}
