package agent

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
	"github.com/IAMI315/Cloud-LocalStore/internal/store"
	syncengine "github.com/IAMI315/Cloud-LocalStore/internal/sync"
	_ "modernc.org/sqlite"
)

type App struct {
	cfg     Config
	db      *sql.DB
	index   *store.ClientStore
	scanner syncengine.Scanner
	logger  *slog.Logger
}

func NewApp(cfg Config, logger *slog.Logger) (*App, error) {
	db, err := sql.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	index, err := store.OpenClient(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &App{cfg: cfg, db: db, index: index, scanner: syncengine.Scanner{FS: filesystem.OSFileSystem{}, Root: cfg.RootPath}, logger: logger}, nil
}

func (a *App) Scan(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := a.scanner.ScanContext(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	records := make([]store.FileRecord, 0, len(entries))
	for _, entry := range entries {
		fileType := "regular"
		if entry.Info.IsDir {
			fileType = "directory"
		} else if !entry.Info.Mode.IsRegular() {
			fileType = "special"
		}
		fileID := entry.Info.PlatformID
		if fileID == "" {
			fileID = "path:" + entry.RelativePath
		}
		records = append(records, store.FileRecord{FileID: fileID, NormalizedPath: entry.RelativePath, FileType: fileType, Size: entry.Info.Size, MTimeNS: entry.Info.ModTime.UnixNano(), PlatformID: entry.Info.PlatformID})
	}
	if err := a.index.ReplaceFileIndex(ctx, records); err != nil {
		return fmt.Errorf("persist scan index: %w", err)
	}
	a.logger.Info("scan complete", "space_id", a.cfg.SpaceID, "entries", len(entries))
	return nil
}

// Run watches the configured root and periodically reconciles the complete
// directory. Events only wake the scanner; the database is updated from a
// complete snapshot to tolerate coalesced or lost OS notifications.
func (a *App) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if err := a.Scan(ctx); err != nil {
		return err
	}
	watcher, err := filesystem.NewWatcher(a.cfg.RootPath)
	if err != nil {
		return err
	}
	defer watcher.Close()
	events := watcher.EventsContext(ctx)
	errors := watcher.Errors()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-events:
			if !ok {
				return nil
			}
			if err := a.Scan(ctx); err != nil && ctx.Err() != nil {
				return err
			}
		case err, ok := <-errors:
			if ok && err != nil {
				a.logger.Warn("filesystem watcher error", "error", err)
			}
		case <-ticker.C:
			if err := a.Scan(ctx); err != nil {
				return err
			}
		}
	}
}
func (a *App) Close() error {
	if a.db == nil {
		return nil
	}
	return a.db.Close()
}
