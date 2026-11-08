package filesystem

import (
	"path/filepath"
	"testing"
)

func TestNormalizeRelativePath(t *testing.T) {
	got, err := NormalizeRelativePath(filepath.Join("dir", "file.txt"))
	if err != nil || got != "dir/file.txt" { t.Fatalf("got %q, %v", got, err) }
	for _, input := range []string{"", ".", "..", "../secret", "/absolute", `..\secret`, `C:\Sync\file.txt`, "C:/Sync/file.txt"} {
		if _, err := NormalizeRelativePath(input); err == nil { t.Errorf("expected error for %q", input) }
	}
}
