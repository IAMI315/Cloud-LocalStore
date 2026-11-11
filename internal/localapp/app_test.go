package localapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScanAcceptsFilesWithoutCurrentVersion(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Init(root, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	app, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.DB.ExecContext(context.Background(), `INSERT INTO files(file_id,normalized_path,file_type,size,mtime_ns,tombstone) VALUES(?,?,?,?,?,0)`, "new-id", "new.txt", "regular", 0, 0); err != nil {
		t.Fatal(err)
	}
	result, err := app.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || result.NewVersions != 1 {
		t.Fatalf("scan result = %+v", result)
	}
}
