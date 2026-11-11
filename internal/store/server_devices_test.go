package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

func TestServerDevicesAreIdempotentAndRevoked(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := OpenServer(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.RegisterDevice(context.Background(), "space", "device", "one")
	if err != nil || d.Status != "active" {
		t.Fatalf("register: %#v %v", d, err)
	}
	d, err = s.RegisterDevice(context.Background(), "space", "device", "renamed")
	if err != nil || d.Name != "renamed" {
		t.Fatalf("retry: %#v %v", d, err)
	}
	if _, err = s.RevokeDevice(context.Background(), "space", "device"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisterDevice(context.Background(), "space", "device", "again"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected revoked, got %v", err)
	}
}
