package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
)

func TestScanner(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := (Scanner{FS: filesystem.OSFileSystem{}, Root: root}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want directory and file", len(entries))
	}
	if entries[0].RelativePath != "docs" || entries[1].RelativePath != "docs/a.txt" {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}
