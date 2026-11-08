package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestScanPersistsIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "state.db")
	app, err := NewApp(Config{RootPath: root, DatabasePath: dbPath, SpaceID: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	count, err := app.index.CountFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("got %d indexed files, want 1", count)
	}
}

func TestScanCancelledDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(Config{RootPath: root, DatabasePath: filepath.Join(t.TempDir(), "state.db"), SpaceID: "test"}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.Scan(ctx); err == nil {
		t.Fatal("expected cancelled scan")
	}
	count, err := app.index.CountFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cancelled scan wrote %d files", count)
	}
}

func TestScanMarksMissingPathAsTombstone(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(Config{RootPath: root, DatabasePath: filepath.Join(t.TempDir(), "state.db"), SpaceID: "test"}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := app.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	active, err := app.index.CountActiveFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("got %d active files after deletion, want 0", active)
	}
	var tombstone int
	if err := app.db.QueryRow(`SELECT tombstone FROM files WHERE normalized_path = 'a.txt'`).Scan(&tombstone); err != nil {
		t.Fatal(err)
	}
	if tombstone != 1 {
		t.Fatalf("got tombstone %d, want 1", tombstone)
	}
}
