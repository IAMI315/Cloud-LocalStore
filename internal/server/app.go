package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/store"
	_ "modernc.org/sqlite"
)

type App struct {
	cfg          Config
	db           *sql.DB
	logger       *slog.Logger
	objects      *objectstore.LocalFS
	pairingToken string
	pairingMu    sync.Mutex
}

func NewApp(cfg Config, logger *slog.Logger) (*App, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8080"
	}
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	if cfg.DatabasePath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o750); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Clean(cfg.DatabasePath))
	if err != nil {
		return nil, err
	}
	if _, err = store.OpenServer(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS server_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if cfg.PairingToken == "" {
		var token string
		err = db.QueryRow(`SELECT value FROM server_settings WHERE key='pairing_token'`).Scan(&token)
		if err != nil || token == "" {
			b := make([]byte, 24)
			if _, err = rand.Read(b); err != nil {
				_ = db.Close()
				return nil, err
			}
			token = hex.EncodeToString(b)
			if _, err = db.Exec(`INSERT OR REPLACE INTO server_settings(key,value) VALUES('pairing_token',?)`, token); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
		cfg.PairingToken = token
	}
	objects, err := objectstore.NewLocalFS(filepath.Join(cfg.DataDir, "objects"))
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &App{cfg: cfg, db: db, logger: logger, objects: objects, pairingToken: cfg.PairingToken}, nil
}

func (a *App) Store() *store.ServerStore {
	return &store.ServerStore{DB: a.db, Objects: a.objects}
}

func (a *App) Ready(ctx context.Context) error {
	if err := a.Store().CheckWritable(ctx); err != nil {
		return err
	}
	probe, err := os.CreateTemp(a.cfg.DataDir, ".ready-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err = probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func (a *App) Handler() http.Handler { return NewService(a).Handler() }

func (a *App) Config() Config       { return a.cfg }
func (a *App) PairingToken() string { return a.pairingToken }
func (a *App) VerifyPairingToken(token string) bool {
	a.pairingMu.Lock()
	defer a.pairingMu.Unlock()
	return token != "" && token == a.pairingToken
}

func (a *App) Close() error {
	if a.db == nil {
		return nil
	}
	return a.db.Close()
}
