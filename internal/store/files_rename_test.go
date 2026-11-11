package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	_ "modernc.org/sqlite"
)

func TestReplaceFileIndexPreservesPlatformIdentityAcrossRename(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenClient(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileIndex(context.Background(), []FileRecord{{FileID: "id", NormalizedPath: "old.txt", FileType: "regular", PlatformID: "dev:1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileIndex(context.Background(), []FileRecord{{FileID: "id", NormalizedPath: "new.txt", FileType: "regular", PlatformID: "dev:1"}}); err != nil {
		t.Fatal(err)
	}
	var path string
	var tombstone int
	if err := db.QueryRow(`SELECT normalized_path, tombstone FROM files WHERE platform_id = 'dev:1'`).Scan(&path, &tombstone); err != nil {
		t.Fatal(err)
	}
	if path != "new.txt" || tombstone != 0 {
		t.Fatalf("got path=%q tombstone=%d, want new.txt/0", path, tombstone)
	}
}

func TestReplaceFileIndexQueuesRemoteDeleteForRename(t *testing.T) {
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
	if err := s.ReplaceFileIndex(ctx, []FileRecord{{FileID: "id", NormalizedPath: "old.txt", FileType: "regular", PlatformID: "dev:1"}}); err != nil {
		t.Fatal(err)
	}
	manifest := objectstore.Manifest{Size: 0, ChunkSize: 4 * 1024 * 1024, HashAlgorithm: "BLAKE3-256", FormatVersion: objectstore.ManifestFormatVersion}
	manifestID, err := s.SaveManifest(ctx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFileVersion(ctx, "id", "", manifestID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileIndex(ctx, []FileRecord{{FileID: "id", NormalizedPath: "new.txt", FileType: "regular", PlatformID: "dev:1"}}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPendingRenameDeletes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Path != "old.txt" || pending[0].ManifestID != string(manifestID) {
		t.Fatalf("pending rename deletes=%+v", pending)
	}
}
