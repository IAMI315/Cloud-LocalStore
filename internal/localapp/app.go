package localapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/store"
	syncengine "github.com/IAMI315/Cloud-LocalStore/internal/sync"
	_ "modernc.org/sqlite"
)

type Config struct {
	RootPath     string `json:"root_path"`
	DataDir      string `json:"data_dir"`
	ServerURL    string `json:"server_url,omitempty"`
	SpaceID      string `json:"space_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	DeviceToken  string `json:"device_token,omitempty"`
	PairingToken string `json:"pairing_token,omitempty"`
}

type App struct {
	Root, DataDir string
	DB            *sql.DB
	Store         *store.ClientStore
	Objects       *objectstore.LocalFS
	syncMu        sync.Mutex
}

type FileVersion struct {
	ID, Parent, Manifest, CreatedAt string
}

func Init(root, dataDir string) (Config, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Config{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return Config{}, fmt.Errorf("sync directory: %w", err)
	}
	if !info.IsDir() {
		return Config{}, fmt.Errorf("sync path must be a directory: %s", root)
	}
	if dataDir == "" {
		dataDir = filepath.Join(root, ".clouddrive")
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("create data directory: %w", err)
	}
	cfg := Config{RootPath: root, DataDir: dataDir}
	if err := writeConfig(filepath.Join(dataDir, "config.json"), cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func LoadConfig(root string) (Config, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Config{}, err
	}
	dataDir := filepath.Join(root, ".clouddrive")
	cfg, err := readConfig(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return Config{}, fmt.Errorf("load local config (run `clouddrive init %q` first): %w", root, err)
	}
	if filepath.Clean(cfg.RootPath) != filepath.Clean(root) {
		return Config{}, fmt.Errorf("config root %q does not match %q", cfg.RootPath, root)
	}
	return cfg, nil
}

func LoadConfigFromData(root, dataDir string) (Config, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Config{}, err
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return Config{}, err
	}
	cfg, err := readConfig(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return Config{}, fmt.Errorf("load local config: %w", err)
	}
	if filepath.Clean(cfg.RootPath) != filepath.Clean(root) {
		return Config{}, fmt.Errorf("config root %q does not match %q", cfg.RootPath, root)
	}
	return cfg, nil
}

func Open(cfg Config) (*App, error) {
	db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "metadata.db"))
	if err != nil {
		return nil, err
	}
	// Opening SQLite can wait on a stale writer/WAL lock. Bound startup so a
	// damaged or concurrently used state directory reports an error instead of
	// making the desktop window appear frozen indefinitely.
	openCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	idx, err := store.OpenClient(openCtx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	objects, err := objectstore.NewLocalFS(filepath.Join(cfg.DataDir, "objects"))
	if err != nil {
		db.Close()
		return nil, err
	}
	return &App{Root: cfg.RootPath, DataDir: cfg.DataDir, DB: db, Store: idx, Objects: objects}, nil
}

func (a *App) Close() error {
	if a.DB != nil {
		return a.DB.Close()
	}
	return nil
}

// Sync performs one cancellable client synchronization pass. Callers may
// invoke it from a timer or watcher trigger; the engine serializes overlapping
// calls and keeps network work out of watcher callbacks.
func (a *App) Sync(ctx context.Context, remote *objectstore.RemoteClient, ids ...string) error {
	if remote == nil {
		return fmt.Errorf("remote client is required")
	}
	spaceID, deviceID := remote.SpaceID, remote.DeviceID
	if len(ids) > 0 {
		spaceID = ids[0]
	}
	if len(ids) > 1 {
		deviceID = ids[1]
	}
	if len(ids) > 2 {
		return fmt.Errorf("at most space and device IDs may be supplied")
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	// Capture local creations/updates before pulling, but do not mark missing
	// paths as deleted yet. A path may be absent temporarily while a remote
	// update is waiting to restore it.
	if _, err := a.scan(ctx, false); err != nil {
		return err
	}
	engine, err := syncengine.NewEngine(a.Root, spaceID, deviceID, a.Objects, a.Store, remote)
	if err != nil {
		return err
	}
	return engine.SyncWithScan(ctx, func(scanCtx context.Context) error {
		_, err := a.scan(scanCtx, true)
		return err
	})
}

func (a *App) SaveConnection(c Config) error {
	c.RootPath, c.DataDir = a.Root, a.DataDir
	return writeConfig(filepath.Join(a.DataDir, "config.json"), c)
}

type ScanResult struct{ Files, NewVersions, Tombstones int }

func (a *App) Scan(ctx context.Context) (ScanResult, error) {
	return a.scan(ctx, true)
}

func (a *App) scan(ctx context.Context, markMissing bool) (ScanResult, error) {
	entries, err := (syncengine.Scanner{FS: filesystem.OSFileSystem{}, Root: a.Root}).ScanContext(ctx)
	if err != nil {
		return ScanResult{}, err
	}
	metaRel, err := filepath.Rel(a.Root, a.DataDir)
	if err == nil && metaRel != "." {
		metaRel = filepath.ToSlash(metaRel)
		filtered := entries[:0]
		for _, e := range entries {
			if e.RelativePath != metaRel && !strings.HasPrefix(e.RelativePath, metaRel+"/") {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	old := make(map[string]fileState)
	rows, err := a.DB.QueryContext(ctx, `SELECT file_id,normalized_path,size,mtime_ns,current_version_id,tombstone FROM files WHERE file_type='regular'`)
	if err != nil {
		return ScanResult{}, err
	}
	for rows.Next() {
		var s fileState
		var currentVersion sql.NullString
		var tomb int
		if err := rows.Scan(&s.ID, &s.Path, &s.Size, &s.MTime, &currentVersion, &tomb); err != nil {
			rows.Close()
			return ScanResult{}, err
		}
		s.Version = currentVersion.String
		s.Tombstone = tomb != 0
		old[s.Path] = s
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ScanResult{}, err
	}
	rows.Close()
	records := make([]store.FileRecord, 0, len(entries))
	for _, e := range entries {
		if e.Info.IsDir || !e.Info.Mode.IsRegular() {
			continue
		}
		id := e.Info.PlatformID
		if id == "" {
			id = "path:" + e.RelativePath
		}
		records = append(records, store.FileRecord{FileID: id, NormalizedPath: e.RelativePath, FileType: "regular", Size: e.Info.Size, MTimeNS: e.Info.ModTime.UnixNano(), PlatformID: e.Info.PlatformID})
	}
	if err := a.Store.ReplaceFileIndexWithOptions(ctx, records, markMissing); err != nil {
		return ScanResult{}, err
	}
	result := ScanResult{Files: len(records)}
	for _, rec := range records {
		prior, exists := old[rec.NormalizedPath]
		if exists && !prior.Tombstone && prior.Size == rec.Size && prior.MTime == rec.MTimeNS && prior.Version != "" {
			continue
		}
		full := filepath.Join(a.Root, filepath.FromSlash(rec.NormalizedPath))
		f, err := os.Open(full)
		if err != nil {
			return result, fmt.Errorf("open %q: %w", rec.NormalizedPath, err)
		}
		chunks := make([]objectstore.Chunk, 0)
		manifest, err := objectstore.BuildManifest(f, domain.DefaultChunkSize, func(c objectstore.Chunk) error {
			if err := a.Objects.Put(ctx, c); err != nil {
				return err
			}
			chunks = append(chunks, c)
			return nil
		})
		closeErr := f.Close()
		if err != nil {
			return result, fmt.Errorf("index %q: %w", rec.NormalizedPath, err)
		}
		if closeErr != nil {
			return result, closeErr
		}
		if err := a.Store.SaveChunks(ctx, chunks); err != nil {
			return result, err
		}
		manifestID, err := a.Objects.PutManifest(ctx, manifest)
		if err != nil {
			return result, err
		}
		if _, err := a.Store.SaveManifest(ctx, manifest); err != nil {
			return result, err
		}
		var current, currentFileID sql.NullString
		if err := a.DB.QueryRowContext(ctx, `SELECT file_id,current_version_id FROM files WHERE normalized_path=?`, rec.NormalizedPath).Scan(&currentFileID, &current); err != nil {
			return result, err
		}
		parent := domain.FileVersionID(current.String)
		if _, err := a.Store.CreateFileVersion(ctx, domain.FileID(currentFileID.String), parent, manifestID, false); err != nil {
			return result, err
		}
		result.NewVersions++
	}
	var tomb int
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM files WHERE file_type='regular' AND tombstone=1`).Scan(&tomb); err == nil {
		result.Tombstones = tomb
	}
	return result, nil
}

type fileState struct {
	ID, Path, Version string
	Size, MTime       int64
	Tombstone         bool
}
type Status struct {
	Root, DataDir                     string
	ActiveFiles, Versions, Tombstones int
	Pending                           int
	LastSyncAt                        string
	TotalBytes, QuotaBytes            int64
	TodayFiles, TodayVersions         int
	IndexUpdatedAt                    string
}

const DefaultQuotaBytes int64 = 5 * 1024 * 1024 * 1024

type FileEntry struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	MTimeNS   int64  `json:"mtime_ns"`
	Version   string `json:"version"`
	Tombstone bool   `json:"tombstone"`
}

// Files returns the indexed regular files, including tombstones retained for
// version history. Paths come directly from the normalized local index.
func (a *App) Files(ctx context.Context) ([]FileEntry, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT normalized_path,size,mtime_ns,COALESCE(current_version_id,''),tombstone FROM files WHERE file_type='regular' ORDER BY normalized_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileEntry
	for rows.Next() {
		var entry FileEntry
		var tomb int
		if err := rows.Scan(&entry.Path, &entry.Size, &entry.MTimeNS, &entry.Version, &tomb); err != nil {
			return nil, err
		}
		entry.Tombstone = tomb != 0
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (a *App) Status(ctx context.Context) (Status, error) {
	s := Status{Root: a.Root, DataDir: a.DataDir}
	for q, d := range map[string]*int{
		`SELECT COUNT(*) FROM files WHERE tombstone=0`:                                              &s.ActiveFiles,
		`SELECT COUNT(*) FROM file_versions`:                                                        &s.Versions,
		`SELECT COUNT(*) FROM files WHERE tombstone=1`:                                              &s.Tombstones,
		`SELECT COUNT(*) FROM files WHERE tombstone=0 AND date(updated_at)=date('now','localtime')`: &s.TodayFiles,
		`SELECT COUNT(*) FROM file_versions WHERE date(created_at)=date('now','localtime')`:         &s.TodayVersions,
	} {
		if err := a.DB.QueryRowContext(ctx, q).Scan(d); err != nil {
			return s, err
		}
	}
	if err := a.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM files WHERE file_type='regular' AND tombstone=0`).Scan(&s.TotalBytes); err != nil {
		return s, err
	}
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE state <> 'committed'`).Scan(&s.Pending); err != nil {
		return s, err
	}
	var pendingRenames int
	if err := a.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_rename_deletes`).Scan(&pendingRenames); err != nil {
		return s, err
	}
	s.Pending += pendingRenames
	if err := a.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated_at),'') FROM sync_state`).Scan(&s.LastSyncAt); err != nil {
		return s, err
	}
	if err := a.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated_at),'') FROM files`).Scan(&s.IndexUpdatedAt); err != nil {
		return s, err
	}
	s.LastSyncAt = localProtocolTime(s.LastSyncAt)
	s.IndexUpdatedAt = localProtocolTime(s.IndexUpdatedAt)
	s.QuotaBytes = DefaultQuotaBytes
	return s, nil
}

// FileStatus compares the current filesystem metadata with the local index
// and outbox so the browser can distinguish synchronized and pending files.
func (a *App) FileStatus(ctx context.Context, path string, size, mtimeNS int64, conflictName bool) (string, error) {
	if conflictName {
		return "conflict", nil
	}
	var indexedSize, indexedMTime int64
	var tombstone, conflict, pending int
	err := a.DB.QueryRowContext(ctx, `
SELECT f.size, f.mtime_ns, f.tombstone,
       EXISTS(SELECT 1 FROM file_versions v WHERE v.version_id=f.current_version_id AND v.is_conflict=1),
       EXISTS(SELECT 1 FROM sync_outbox o WHERE o.normalized_path=? AND o.state <> 'committed') OR
       EXISTS(SELECT 1 FROM pending_rename_deletes p WHERE p.normalized_path=?)
FROM files f WHERE f.normalized_path=? AND f.file_type='regular'`, path, path, path).
		Scan(&indexedSize, &indexedMTime, &tombstone, &conflict, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", nil
	}
	if err != nil {
		return "", err
	}
	if conflict != 0 {
		return "conflict", nil
	}
	if tombstone != 0 || pending != 0 || indexedSize != size || indexedMTime != mtimeNS {
		return "pending", nil
	}
	return "synced", nil
}

func (a *App) Versions(ctx context.Context, path string) ([]FileVersion, error) {
	rel, err := normalizeUserPath(path)
	if err != nil {
		return nil, err
	}
	var fileID string
	if err := a.DB.QueryRowContext(ctx, `SELECT file_id FROM files WHERE normalized_path=?`, rel).Scan(&fileID); err != nil {
		return nil, err
	}
	rows, err := a.DB.QueryContext(ctx, `SELECT version_id,COALESCE(parent_version_id,''),manifest_id,created_at FROM file_versions WHERE file_id=? ORDER BY created_at,version_id`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileVersion
	for rows.Next() {
		var v FileVersion
		if err := rows.Scan(&v.ID, &v.Parent, &v.Manifest, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.CreatedAt = localProtocolTime(v.CreatedAt)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (a *App) Restore(ctx context.Context, path, version string) (string, error) {
	rel, err := normalizeUserPath(path)
	if err != nil {
		return "", err
	}
	var fileID string
	if err := a.DB.QueryRowContext(ctx, `SELECT file_id FROM files WHERE normalized_path=?`, rel).Scan(&fileID); err != nil {
		return "", err
	}
	vid, err := a.Store.RestoreFileVersionTo(ctx, domain.FileID(fileID), domain.FileVersionID(version), filepath.Join(a.Root, filepath.FromSlash(rel)), a.Objects)
	return string(vid), err
}

func (a *App) Run(ctx context.Context, interval time.Duration) error {
	return a.runMonitor(ctx, interval, func(runCtx context.Context) error {
		_, err := a.Scan(runCtx)
		return err
	})
}

// RunSync watches the local library and runs a bidirectional sync whenever a
// filesystem event arrives. Network errors are intentionally retried on the
// next event or periodic tick so a temporary outage does not stop monitoring.
func (a *App) RunSync(ctx context.Context, remote *objectstore.RemoteClient, interval time.Duration) error {
	if remote == nil {
		return fmt.Errorf("remote client is required")
	}
	return a.runMonitor(ctx, interval, func(runCtx context.Context) error {
		// A network outage must not leave the monitor blocked forever. Keep the
		// parent monitor context separate so a timed-out attempt is retried on
		// the next filesystem event or polling tick.
		attemptCtx, cancel := context.WithTimeout(runCtx, 30*time.Second)
		err := a.Sync(attemptCtx, remote)
		cancel()
		if runCtx.Err() != nil {
			return runCtx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return nil
	})
}

func (a *App) runMonitor(ctx context.Context, interval time.Duration, onTrigger func(context.Context) error) error {
	watcher, err := filesystem.NewWatcherWithExcludes(a.Root, []string{a.DataDir})
	if err != nil {
		return err
	}
	defer watcher.Close()
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if err := onTrigger(ctx); err != nil {
		return err
	}
	events := watcher.EventsContext(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-events:
			if !ok {
				return nil
			}
			if err := onTrigger(ctx); err != nil && ctx.Err() == nil {
				return err
			}
		case err := <-watcher.Errors():
			if err != nil {
				return err
			}
		case <-ticker.C:
			if err := onTrigger(ctx); err != nil {
				return err
			}
		}
	}
}

func normalizeUserPath(p string) (string, error) {
	rel, err := filesystem.NormalizeRelativePath(filepath.FromSlash(p))
	if err != nil {
		return "", fmt.Errorf("invalid path %q: use a path inside the sync directory", p)
	}
	return rel, nil
}
func writeConfig(path string, c Config) error {
	b, err := jsonMarshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
func readConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := jsonUnmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var jsonMarshal = func(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
var jsonUnmarshal = func(b []byte, v any) error { return json.Unmarshal(b, v) }
