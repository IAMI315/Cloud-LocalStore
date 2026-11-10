package store

import (
	"context"
	"database/sql"
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
	if count != 2 {
		t.Fatalf("migration count = %d, want 2", count)
	}
}
