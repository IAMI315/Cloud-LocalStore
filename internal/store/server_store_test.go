package store

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenServerUsesServerSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil { t.Fatal(err) }
	defer db.Close()
	if _, err := OpenServer(context.Background(), db); err != nil { t.Fatal(err) }
	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM server_schema_migrations`).Scan(&count); err != nil { t.Fatal(err) }
	if _, err := db.Exec(`SELECT COUNT(1) FROM files`); err == nil { t.Fatal("server schema unexpectedly created client files table") }
}
