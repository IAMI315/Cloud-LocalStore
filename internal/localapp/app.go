package localapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/domain"
	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/store"
	syncengine "github.com/IAMI315/Cloud-LocalStore/internal/sync"
	_ "modernc.org/sqlite"
)

type Config struct {
	RootPath string `json:"root_path"`
	DataDir  string `json:"data_dir"`
}

type App struct {
	Root, DataDir string
	DB            *sql.DB
	Store         *store.ClientStore
	Objects       *objectstore.LocalFS
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
	idx, err := store.OpenClient(context.Background(), db)
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

type ScanResult struct{ Files, NewVersions, Tombstones int }

func (a *App) Scan(ctx context.Context) (ScanResult, error) {
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
		var tomb int
		if err := rows.Scan(&s.ID, &s.Path, &s.Size, &s.MTime, &s.Version, &tomb); err != nil {
			rows.Close()
			return ScanResult{}, err
		}
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
	if err := a.Store.ReplaceFileIndex(ctx, records); err != nil {
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
		var current sql.NullString
		if err := a.DB.QueryRowContext(ctx, `SELECT current_version_id FROM files WHERE normalized_path=?`, rec.NormalizedPath).Scan(&current); err != nil {
			return result, err
		}
		parent := domain.FileVersionID(current.String)
		if _, err := a.Store.CreateFileVersion(ctx, domain.FileID(rec.FileID), parent, manifestID, false); err != nil {
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
}

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
	for q, d := range map[string]*int{`SELECT COUNT(*) FROM files WHERE tombstone=0`: &s.ActiveFiles, `SELECT COUNT(*) FROM file_versions`: &s.Versions, `SELECT COUNT(*) FROM files WHERE tombstone=1`: &s.Tombstones} {
		if err := a.DB.QueryRowContext(ctx, q).Scan(d); err != nil {
			return s, err
		}
	}
	return s, nil
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
	watcher, err := filesystem.NewWatcherWithExcludes(a.Root, []string{a.DataDir})
	if err != nil {
		return err
	}
	defer watcher.Close()
	if _, err := a.Scan(ctx); err != nil {
		return err
	}
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	events := watcher.EventsContext(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-events:
			if !ok {
				return nil
			}
			if _, err := a.Scan(ctx); err != nil && ctx.Err() == nil {
				return err
			}
		case err := <-watcher.Errors():
			if err != nil {
				return err
			}
		case <-ticker.C:
			if _, err := a.Scan(ctx); err != nil {
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
