package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	_ "modernc.org/sqlite"
)

func TestReplaceFileIndexIsAtomic(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileIndex(context.Background(), []FileRecord{{FileID: "id", NormalizedPath: "a.txt", FileType: "regular"}}); err != nil {
		t.Fatal(err)
	}
	count, err := s.CountFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("got %d files, want 1", count)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ReplaceFileIndex(ctx, []FileRecord{{FileID: "id2", NormalizedPath: "b.txt", FileType: "regular"}}); err == nil {
		t.Fatal("expected cancelled transaction")
	}
	count, err = s.CountFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cancelled write changed index: got %d files", count)
	}
}

func TestReplaceFileIndexWithoutMarkingMissing(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.ReplaceFileIndex(ctx, []FileRecord{{FileID: "id", NormalizedPath: "remote.txt", FileType: "regular"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileIndexWithOptions(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	var tombstone int
	if err := db.QueryRowContext(ctx, `SELECT tombstone FROM files WHERE normalized_path=?`, "remote.txt").Scan(&tombstone); err != nil {
		t.Fatal(err)
	}
	if tombstone != 0 {
		t.Fatalf("pre-pull scan marked missing path deleted: %d", tombstone)
	}
}

func TestUpsertFilePreservesIdentityReferencedByVersions(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.ReplaceFileIndex(ctx, []FileRecord{{FileID: "local-id", NormalizedPath: "hello.txt", FileType: "regular"}}); err != nil {
		t.Fatal(err)
	}
	manifest := objectstore.Manifest{Size: 0, ChunkSize: 4 * 1024 * 1024, HashAlgorithm: "BLAKE3-256", FormatVersion: objectstore.ManifestFormatVersion}
	manifestID, err := s.SaveManifest(ctx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFileVersion(ctx, "local-id", "", manifestID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFile(ctx, FileRecord{FileID: "remote-path-id", NormalizedPath: "hello.txt", FileType: "regular"}); err != nil {
		t.Fatal(err)
	}
	var fileID string
	if err := db.QueryRowContext(ctx, `SELECT file_id FROM files WHERE normalized_path=?`, "hello.txt").Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if fileID != "local-id" {
		t.Fatalf("file identity changed to %q", fileID)
	}
	var versions int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM file_versions WHERE file_id=?`, "local-id").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("historical versions = %d, want 1", versions)
	}
}

func TestReplaceFileIndexPreservesIdentityReferencedByVersions(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.ReplaceFileIndex(ctx, []FileRecord{{FileID: "local-id", NormalizedPath: "hello.txt", FileType: "regular"}}); err != nil {
		t.Fatal(err)
	}
	manifest := objectstore.Manifest{Size: 0, ChunkSize: 4 * 1024 * 1024, HashAlgorithm: "BLAKE3-256", FormatVersion: objectstore.ManifestFormatVersion}
	manifestID, err := s.SaveManifest(ctx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFileVersion(ctx, "local-id", "", manifestID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileIndex(ctx, []FileRecord{{FileID: "remote-path-id", NormalizedPath: "hello.txt", FileType: "regular", Size: 4}}); err != nil {
		t.Fatal(err)
	}
	var fileID string
	if err := db.QueryRowContext(ctx, `SELECT file_id FROM files WHERE normalized_path=?`, "hello.txt").Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if fileID != "local-id" {
		t.Fatalf("file identity changed to %q", fileID)
	}
}
