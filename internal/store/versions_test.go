package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	_ "modernc.org/sqlite"
)

func TestSaveManifestAndRestoreVersion(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO files(file_id,normalized_path,file_type) VALUES('f','a.txt','regular')`); err != nil {
		t.Fatal(err)
	}
	m := objectstore.Manifest{Size: 5, ChunkSize: 4, Chunks: []domain.ChunkHash{objectstore.Hash([]byte("hell")), objectstore.Hash([]byte("o"))}, HashAlgorithm: "BLAKE3-256", FormatVersion: objectstore.ManifestFormatVersion}
	id, err := s.SaveManifest(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.CreateFileVersion(context.Background(), "f", "", id, false)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.RestoreFileVersion(context.Background(), "f", v1)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 {
		t.Fatal("restore should create a new version")
	}
}

func TestRestoreFileVersionToRebuildsFileBeforeCreatingVersion(t *testing.T) {
	root := t.TempDir()
	objects, err := objectstore.NewLocalFS(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "input.bin")
	if err := os.WriteFile(input, []byte("abcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := objectstore.BuildFile(context.Background(), input, objects, 4)
	if err != nil {
		t.Fatal(err)
	}
	manifestID, err := objects.PutManifest(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO files(file_id,normalized_path,file_type) VALUES('f','a.txt','regular')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	v1, err := s.CreateFileVersion(context.Background(), "f", "", manifestID, false)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "restored", "a.txt")
	v2, err := s.RestoreFileVersionTo(context.Background(), "f", v1, target, objects)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 {
		t.Fatal("restore should create a new version")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdefgh" {
		t.Fatalf("restored %q", got)
	}
}

func TestRestoreFileVersionToLeavesVersionAndFileOnMissingChunk(t *testing.T) {
	root := t.TempDir()
	objects, err := objectstore.NewLocalFS(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "input.bin")
	if err := os.WriteFile(input, []byte("abcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := objectstore.BuildFile(context.Background(), input, objects, 4)
	if err != nil {
		t.Fatal(err)
	}
	manifestID, err := objects.PutManifest(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(objects.Root, string(manifest.Chunks[1][:2]), string(manifest.Chunks[1]))); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO files(file_id,normalized_path,file_type) VALUES('f','a.txt','regular')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveManifest(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	v1, err := s.CreateFileVersion(context.Background(), "f", "", manifestID, false)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "a.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFileVersionTo(context.Background(), "f", v1, target, objects); err == nil {
		t.Fatal("expected missing chunk error")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("target changed after failed restore: %q", got)
	}
	var current string
	if err := db.QueryRow(`SELECT current_version_id FROM files WHERE file_id='f'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != string(v1) {
		t.Fatalf("current version changed after failed restore: %q", current)
	}
}

func TestCreateFileVersionRejectsMissingReferences(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFileVersion(context.Background(), "missing", "", "manifest", false); err == nil {
		t.Fatal("expected missing file error")
	}
}

func TestClientMigrationsAreIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := ApplyMigrations(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) {
		t.Fatalf("migration count = %d, want %d", count, len(migrations))
	}
}
