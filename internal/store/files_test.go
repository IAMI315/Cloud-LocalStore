package store

import (
	"context"
	"database/sql"
	"testing"

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
