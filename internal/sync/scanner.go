package sync

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
)

type Entry struct { RelativePath string; Info filesystem.FileInfo }
type Scanner struct { FS filesystem.FileSystem; Root string }

func (s Scanner) Scan() ([]Entry, error) {
	return s.ScanContext(context.Background())
}

func (s Scanner) ScanContext(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	err := s.FS.Walk(s.Root, func(info filesystem.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.Path == s.Root { return nil }
		rel, err := filepath.Rel(s.Root, info.Path); if err != nil { return err }
		normalized, err := filesystem.NormalizeRelativePath(rel); if err != nil { return err }
		entries = append(entries, Entry{RelativePath: normalized, Info: info})
		return nil
	})
	if err != nil { return nil, err }
	sort.Slice(entries, func(i, j int) bool { return entries[i].RelativePath < entries[j].RelativePath })
	return entries, nil
}
