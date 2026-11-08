package store

import (
	"context"
	"database/sql"
	"testing"

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
