package store

import (
	"context"
	"database/sql"
	"fmt"
)

type ClientStore struct{ DB *sql.DB }

func OpenClient(ctx context.Context, db *sql.DB) (*ClientStore, error) {
	for _, pragma := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return nil, fmt.Errorf("configure sqlite (%s): %w", pragma, err)
		}
	}
	if err := ApplyMigrations(ctx, db); err != nil {
		return nil, err
	}
	return &ClientStore{DB: db}, nil
}
