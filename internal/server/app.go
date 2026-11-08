package server

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/IAMI315/Cloud-LocalStore/internal/store"
	_ "modernc.org/sqlite"
)

type App struct { cfg Config; db *sql.DB; logger *slog.Logger }

func NewApp(cfg Config, logger *slog.Logger) (*App, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil { return nil, err }
	db, err := sql.Open("sqlite", filepath.Clean(cfg.DatabasePath)); if err != nil { return nil, err }
	if _, err = store.OpenServer(context.Background(), db); err != nil { _ = db.Close(); return nil, err }
	return &App{cfg: cfg, db: db, logger: logger}, nil
}

func (a *App) Close() error { if a.db == nil { return nil }; return a.db.Close() }
