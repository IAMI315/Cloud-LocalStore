package sync

import (
	"path/filepath"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
)

func TestScannerReturnsMissingRootError(t *testing.T) {
	_, err := (Scanner{FS: filesystem.OSFileSystem{}, Root: filepath.Join(t.TempDir(), "missing")}).Scan()
	if err == nil { t.Fatal("expected missing root error") }
}
