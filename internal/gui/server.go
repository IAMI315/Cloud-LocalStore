package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
	"github.com/IAMI315/Cloud-LocalStore/internal/localapp"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	syncengine "github.com/IAMI315/Cloud-LocalStore/internal/sync"
)

const (
	defaultAddr            = "127.0.0.1:8765"
	remoteSyncPollInterval = 2 * time.Second
	// Let the native window and first state request render before the initial
	// full scan/network reconciliation starts in the background.
	remoteMonitorStartupDelay = 1500 * time.Millisecond
)

// ErrFolderSelectionUnsupported is returned when the host cannot provide a
// native system folder picker.
var ErrFolderSelectionUnsupported = errors.New("当前平台不支持系统文件夹选择，请手动填写路径")

type folderSelector func() (string, error)

type Server struct {
	mu            sync.RWMutex
	operations    sync.Mutex
	app           *localapp.App
	cfg           localapp.Config
	monitorCancel context.CancelFunc
	monitorDone   chan struct{}
	addr          string
	httpServer    *http.Server
	static        fs.FS
	selectFolder  folderSelector
}

func New(addr string) *Server {
	if strings.TrimSpace(addr) == "" {
		addr = defaultAddr
	}
	static, _ := fs.Sub(webFiles, "web")
	s := &Server{addr: addr, static: static, selectFolder: selectFolder}
	return s
}

// OpenDefault loads the directory created beside the desktop executable. It
// initializes the directory only on the first launch, preserving the saved
// connection and device credentials on subsequent launches.
func (s *Server) OpenDefault(root, dataDir string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	cfg, err := localapp.LoadConfigFromData(root, dataDir)
	if errors.Is(err, os.ErrNotExist) {
		cfg, err = localapp.Init(root, dataDir)
	}
	if err != nil {
		return err
	}
	app, err := localapp.Open(cfg)
	if err != nil {
		return err
	}
	s.operations.Lock()
	s.mu.Lock()
	s.stopMonitorLocked()
	old := s.app
	s.app, s.cfg = app, cfg
	s.mu.Unlock()
	s.operations.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// StartConfiguredMonitor starts the saved remote monitor after the desktop
// shell has finished opening the local database.
func (s *Server) StartConfiguredMonitor() { s.ensureRemoteMonitor() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/library", s.handleLibrary)
	mux.HandleFunc("/api/select-folder", s.handleSelectFolder)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/files", s.handleFiles)
	mux.HandleFunc("/api/browser", s.handleBrowser)
	mux.HandleFunc("/api/conflicts", s.handleConflicts)
	mux.HandleFunc("/api/conflicts/resolve", s.handleConflictResolve)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/file", s.handleFileAction)
	mux.HandleFunc("/api/versions", s.handleVersions)
	mux.HandleFunc("/api/restore", s.handleRestore)
	mux.HandleFunc("/api/monitor", s.handleMonitor)
	mux.HandleFunc("/api/connection", s.handleConnection)
	mux.HandleFunc("/api/sync", s.handleSync)
	mux.Handle("/", http.FileServer(http.FS(s.static)))
	return mux
}

func (s *Server) handleSelectFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	selected, err := s.selectFolder()
	if errors.Is(err, ErrFolderSelectionUnsupported) {
		writeError(w, http.StatusNotImplemented, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if strings.TrimSpace(selected) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"selected": false, "cancelled": true, "path": ""})
		return
	}
	absolute, err := filepath.Abs(selected)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("resolve selected folder: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"selected": true, "cancelled": false, "path": filepath.Clean(absolute)})
}

func (s *Server) Listen() (net.Listener, error) {
	if err := validateLoopbackAddr(s.addr); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.httpServer = &http.Server{Handler: s.Handler()}
	s.addr = listener.Addr().String()
	s.mu.Unlock()
	return listener, nil
}

func validateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("GUI address must include a port and bind to loopback: %w", err)
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return errors.New("GUI must bind to a loopback address, not all interfaces")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("GUI address %q is not a loopback address", host)
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("GUI address %q is not a loopback address", host)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("GUI address %q resolves outside loopback", host)
		}
	}
	return nil
}

func (s *Server) Serve(listener net.Listener) error {
	s.mu.RLock()
	server := s.httpServer
	s.mu.RUnlock()
	if server == nil {
		return errors.New("GUI server has not been started with Listen")
	}
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) URL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return "http://" + s.addr + "/"
}

func (s *Server) Shutdown(ctx context.Context) error {
	// Stop background reconciliation before waiting for HTTP handlers. This
	// releases SQLite/network work that state and browser requests may be
	// waiting behind during tray exit.
	s.stopMonitorForShutdown()
	s.mu.RLock()
	server := s.httpServer
	s.mu.RUnlock()
	var err error
	if server != nil {
		err = server.Shutdown(ctx)
	}
	if closeErr := s.Close(); err == nil {
		err = closeErr
	}
	return err
}

// stopMonitorForShutdown is safe to call without the operations mutex. It is
// intentionally separate from stopMonitorLocked because Shutdown must cancel
// the monitor before waiting for in-flight HTTP requests.
func (s *Server) stopMonitorForShutdown() {
	s.mu.Lock()
	cancel, done := s.monitorCancel, s.monitorDone
	s.monitorCancel, s.monitorDone = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *Server) Close() error {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	cancel, done := s.monitorCancel, s.monitorDone
	s.monitorCancel, s.monitorDone = nil, nil
	app := s.app
	s.app = nil
	s.cfg = localapp.Config{}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if app == nil {
		return nil
	}
	return app.Close()
}

type libraryRequest struct {
	Root       string `json:"root"`
	DataDir    string `json:"data_dir"`
	Initialize bool   `json:"initialize"`
}

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req libraryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	root, err := filepath.Abs(strings.TrimSpace(req.Root))
	if err != nil || strings.TrimSpace(req.Root) == "" {
		writeError(w, http.StatusBadRequest, errors.New("root must be a local directory path"))
		return
	}
	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		if statErr != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("sync directory: %w", statErr))
		} else {
			writeError(w, http.StatusBadRequest, errors.New("sync path must be a directory"))
		}
		return
	}
	dataDir := strings.TrimSpace(req.DataDir)
	if dataDir != "" {
		dataDir, err = filepath.Abs(dataDir)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("invalid data directory path"))
			return
		}
	}
	var cfg localapp.Config
	if req.Initialize {
		cfg, err = localapp.Init(root, dataDir)
	} else if dataDir == "" {
		cfg, err = localapp.LoadConfig(root)
	} else {
		cfg, err = localapp.LoadConfigFromData(root, dataDir)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	app, err := localapp.Open(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.operations.Lock()
	s.mu.Lock()
	s.stopMonitorLocked()
	old := s.app
	s.app, s.cfg = app, cfg
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	s.operations.Unlock()
	s.ensureRemoteMonitor()
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "root": cfg.RootPath, "data_dir": cfg.DataDir})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.RLock()
	app, cfg := s.app, s.cfg
	s.mu.RUnlock()
	result := map[string]any{"configured": app != nil, "network": "未配置"}
	if app == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	status, err := app.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	files, err := app.Files(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result["root"], result["data_dir"] = cfg.RootPath, cfg.DataDir
	result["server_url"], result["space_id"], result["device_id"] = cfg.ServerURL, cfg.SpaceID, cfg.DeviceID
	if cfg.ServerURL != "" && cfg.DeviceToken != "" {
		result["network"] = "已配置凭证"
	}
	result["status"], result["files"] = status, files
	s.mu.RLock()
	result["monitoring"] = s.monitorCancel != nil
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, result)
}

type connectionRequest struct {
	ServerURL    string `json:"server_url"`
	SpaceID      string `json:"space_id"`
	DeviceID     string `json:"device_id"`
	DeviceToken  string `json:"device_token"`
	PairingToken string `json:"pairing_token"`
}

func (s *Server) handleConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req connectionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.withApp(func(app *localapp.App) error {
		s.mu.RLock()
		cfg := s.cfg
		s.mu.RUnlock()
		previousDeviceID := cfg.DeviceID
		cfg.ServerURL, cfg.SpaceID, cfg.DeviceID = strings.TrimSpace(req.ServerURL), strings.TrimSpace(req.SpaceID), strings.TrimSpace(req.DeviceID)
		if cfg.ServerURL == "" || cfg.SpaceID == "" || cfg.DeviceID == "" {
			return errors.New("server_url, space_id and device_id are required")
		}
		// Secret fields are intentionally not repopulated into the page. An empty
		// device-token field therefore means "keep the saved credential", except
		// when a pairing code is supplied or the device identity changed. Both are
		// explicit signals that the user wants a fresh device credential.
		pairingProvided := false
		if token := strings.TrimSpace(req.DeviceToken); token != "" {
			cfg.DeviceToken = token
		}
		if token := strings.TrimSpace(req.PairingToken); token != "" {
			cfg.PairingToken = token
			pairingProvided = true
		}
		if strings.TrimSpace(req.DeviceToken) == "" && (pairingProvided || (previousDeviceID != "" && previousDeviceID != cfg.DeviceID)) {
			cfg.DeviceToken = ""
		}
		if cfg.DeviceToken == "" && cfg.PairingToken != "" {
			remote := &objectstore.RemoteClient{BaseURL: cfg.ServerURL, SpaceID: cfg.SpaceID, DeviceID: cfg.DeviceID}
			paired, err := remote.RegisterDevice(r.Context(), cfg.DeviceID, cfg.PairingToken)
			if err != nil {
				return fmt.Errorf("pair device: %w", err)
			}
			cfg.DeviceToken = paired.DeviceToken
			if cfg.DeviceToken == "" {
				return errors.New("server returned an empty device token")
			}
		}
		if err := app.SaveConnection(cfg); err != nil {
			return err
		}
		s.mu.Lock()
		s.cfg = cfg
		s.mu.Unlock()
		return nil
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	s.restartRemoteMonitor()
	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	err := s.withApp(func(app *localapp.App) error {
		s.mu.RLock()
		cfg := s.cfg
		s.mu.RUnlock()
		if cfg.ServerURL == "" || cfg.DeviceToken == "" {
			return errors.New("连接凭证未配置")
		}
		remote := &objectstore.RemoteClient{BaseURL: cfg.ServerURL, SpaceID: cfg.SpaceID, DeviceID: cfg.DeviceID, DeviceToken: cfg.DeviceToken}
		if err := app.Sync(r.Context(), remote); err != nil {
			if strings.Contains(err.Error(), "remote unauthorized") {
				return errors.New("设备授权已失效，请输入配对码重新保存连接，并确保每台电脑使用不同的设备 ID")
			}
			return err
		}
		return nil
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"synced": true})
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var result localapp.ScanResult
	err := s.withApp(func(app *localapp.App) error {
		var err error
		result, err = app.Scan(r.Context())
		return err
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	var files []localapp.FileEntry
	err := s.withApp(func(app *localapp.App) error {
		var err error
		files, err = app.Files(r.Context())
		return err
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

type browserEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size"`
	Modified  string `json:"modified"`
	Kind      string `json:"kind"`
	Conflict  bool   `json:"conflict"`
}

type conflictEntry struct {
	Path         string `json:"path"`
	OriginalPath string `json:"original_path"`
	Size         int64  `json:"size"`
	Modified     string `json:"modified"`
}

var conflictNamePattern = regexp.MustCompile(`^(.*)\.conflict-[A-Za-z0-9_-]+-r[0-9]+(?:-[0-9]+)?(\.[^.]*)?$`)

func (s *Server) handleBrowser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	var result struct {
		Path    string         `json:"path"`
		Parent  string         `json:"parent"`
		Entries []browserEntry `json:"entries"`
	}
	err := s.withApp(func(app *localapp.App) error {
		rel, target, err := safeLocalPath(app.Root, r.URL.Query().Get("path"), true)
		if err != nil {
			return err
		}
		info, err := os.Stat(target)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("path is not a directory")
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			return err
		}
		query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
		for _, entry := range entries {
			name := entry.Name()
			if query != "" && !strings.Contains(strings.ToLower(name), query) {
				continue
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return err
			}
			entryPath := name
			if rel != "" {
				entryPath = filepath.ToSlash(filepath.Join(rel, name))
			}
			kind := "file"
			if entry.IsDir() {
				kind = "folder"
			} else if strings.HasPrefix(mime.TypeByExtension(strings.ToLower(filepath.Ext(name))), "image/") {
				kind = "image"
			} else if isTextFile(name) {
				kind = "text"
			}
			result.Entries = append(result.Entries, browserEntry{Name: name, Path: entryPath, Directory: entry.IsDir(), Size: entryInfo.Size(), Modified: entryInfo.ModTime().Format(time.RFC3339), Kind: kind, Conflict: isConflictName(name)})
		}
		field := strings.ToLower(r.URL.Query().Get("sort"))
		desc := strings.EqualFold(r.URL.Query().Get("order"), "desc")
		sort.SliceStable(result.Entries, func(i, j int) bool {
			a, b := result.Entries[i], result.Entries[j]
			if a.Directory != b.Directory {
				return a.Directory
			}
			var less bool
			switch field {
			case "size":
				less = a.Size < b.Size
			case "modified":
				less = a.Modified < b.Modified
			default:
				less = strings.ToLower(a.Name) < strings.ToLower(b.Name)
			}
			if desc {
				return !less && a.Name != b.Name
			}
			return less
		})
		result.Path = rel
		if rel != "" {
			result.Parent = filepath.ToSlash(filepath.Dir(rel))
			if result.Parent == "." {
				result.Parent = ""
			}
		}
		return nil
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	if result.Entries == nil {
		result.Entries = []browserEntry{}
	}
	writeJSON(w, http.StatusOK, result)
}

func isConflictName(name string) bool {
	return strings.Contains(strings.ToLower(name), ".conflict-")
}

func conflictOriginalPath(path string) (string, bool) {
	dir, name := filepath.Split(filepath.FromSlash(path))
	matches := conflictNamePattern.FindStringSubmatch(name)
	if matches == nil {
		return "", false
	}
	return filepath.ToSlash(filepath.Join(dir, matches[1]+matches[2])), true
}

func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	var conflicts []conflictEntry
	err := s.withApp(func(app *localapp.App) error {
		entries, err := (syncengine.Scanner{FS: filesystem.OSFileSystem{}, Root: app.Root}).ScanContext(r.Context())
		if err != nil {
			return err
		}
		metaRel, err := filepath.Rel(app.Root, app.DataDir)
		if err == nil && metaRel != "." {
			metaRel = filepath.ToSlash(metaRel)
		}
		for _, entry := range entries {
			if entry.Info.IsDir || !entry.Info.Mode.IsRegular() || !isConflictName(entry.RelativePath) {
				continue
			}
			if metaRel != "." && (entry.RelativePath == metaRel || strings.HasPrefix(entry.RelativePath, metaRel+"/")) {
				continue
			}
			original, ok := conflictOriginalPath(entry.RelativePath)
			if !ok {
				continue
			}
			conflicts = append(conflicts, conflictEntry{Path: entry.RelativePath, OriginalPath: original, Size: entry.Info.Size, Modified: entry.Info.ModTime.Format(time.RFC3339)})
		}
		sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
		return nil
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	if conflicts == nil {
		conflicts = []conflictEntry{}
	}
	writeJSON(w, http.StatusOK, conflicts)
}

type conflictResolveRequest struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

func (s *Server) handleConflictResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req conflictResolveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Action != "keep_local" && req.Action != "keep_remote" {
		writeError(w, http.StatusBadRequest, errors.New("action must be keep_local or keep_remote"))
		return
	}
	err := s.withApp(func(app *localapp.App) error {
		rel, conflictTarget, err := safeLocalPath(app.Root, req.Path, false)
		if err != nil {
			return err
		}
		original, ok := conflictOriginalPath(rel)
		if !ok {
			return errors.New("path is not a recognized conflict file")
		}
		_, originalTarget, err := safeLocalPath(app.Root, original, false)
		if err != nil {
			return err
		}
		info, err := os.Stat(conflictTarget)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("conflict path must be a regular file")
		}
		if req.Action == "keep_remote" {
			return os.Remove(conflictTarget)
		}
		if _, err := os.Stat(originalTarget); errors.Is(err, os.ErrNotExist) {
			return os.Rename(conflictTarget, originalTarget)
		} else if err != nil {
			return err
		}
		parent := filepath.Dir(originalTarget)
		temp, err := os.CreateTemp(parent, ".clouddrive-resolve-*")
		if err != nil {
			return err
		}
		tempPath := temp.Name()
		if err := temp.Close(); err != nil {
			_ = os.Remove(tempPath)
			return err
		}
		if err := os.Remove(tempPath); err != nil {
			return err
		}
		if err := os.Rename(originalTarget, tempPath); err != nil {
			return err
		}
		if err := os.Rename(conflictTarget, originalTarget); err != nil {
			_ = os.Rename(tempPath, originalTarget)
			return err
		}
		return os.Remove(tempPath)
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	err := s.withApp(func(app *localapp.App) error {
		_, target, err := safeLocalPath(app.Root, r.URL.Query().Get("path"), false)
		if err != nil {
			return err
		}
		info, err := os.Stat(target)
		if err != nil {
			return err
		}
		if info.IsDir() || info.Size() > 8<<20 {
			return errors.New("file cannot be previewed")
		}
		f, err := os.Open(target)
		if err != nil {
			return err
		}
		defer f.Close()
		contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(target)))
		if contentType == "" {
			buf := make([]byte, 512)
			n, _ := f.Read(buf)
			contentType = http.DetectContentType(buf[:n])
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", "inline; filename=\""+filepath.Base(target)+"\"")
		_, err = io.CopyN(w, f, info.Size())
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	})
	if err != nil {
		writeAppError(w, err)
	}
}

type fileActionRequest struct {
	Action string   `json:"action"`
	Path   string   `json:"path"`
	Name   string   `json:"name"`
	Target string   `json:"target"`
	Paths  []string `json:"paths"`
}

func (s *Server) handleFileAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req fileActionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.withApp(func(app *localapp.App) error {
		action := strings.ToLower(strings.TrimSpace(req.Action))
		path := strings.TrimSpace(req.Path)
		switch action {
		case "mkdir", "create":
			if strings.TrimSpace(req.Name) == "" {
				return errors.New("name is required")
			}
			_, parent, err := safeLocalPath(app.Root, path, true)
			if err != nil {
				return err
			}
			if err := validateSimpleName(req.Name); err != nil {
				return err
			}
			if action == "mkdir" {
				return os.Mkdir(filepath.Join(parent, req.Name), 0o700)
			}
			f, err := os.OpenFile(filepath.Join(parent, req.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			return f.Close()
		case "rename":
			if err := validateSimpleName(req.Name); err != nil {
				return err
			}
			_, source, err := safeLocalPath(app.Root, path, false)
			if err != nil {
				return err
			}
			return os.Rename(source, filepath.Join(filepath.Dir(source), req.Name))
		case "delete":
			paths := req.Paths
			if len(paths) == 0 {
				paths = []string{path}
			}
			for _, candidate := range paths {
				_, target, err := safeLocalPath(app.Root, candidate, false)
				if err != nil {
					return err
				}
				if err := os.RemoveAll(target); err != nil {
					return err
				}
			}
			return nil
		default:
			return errors.New("action must be mkdir, create, rename or delete")
		}
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func safeLocalPath(root, relative string, directory bool) (string, string, error) {
	relative = strings.TrimSpace(filepath.ToSlash(relative))
	if relative == "." {
		relative = ""
	}
	rel := ""
	if relative != "" {
		var err error
		rel, err = filesystem.NormalizeRelativePath(filepath.FromSlash(relative))
		if err != nil {
			return "", "", errors.New("invalid path")
		}
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	cleanRoot := filepath.Clean(root)
	resolved, err := filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	relToRoot, err := filepath.Rel(cleanRoot, resolved)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", "", errors.New("path escapes sync directory")
	}
	current := cleanRoot
	for _, part := range strings.Split(relToRoot, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", "", errors.New("symlink paths are not allowed")
		}
		if statErr != nil && os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", "", statErr
		}
	}
	if directory {
		return rel, target, nil
	}
	return rel, target, nil
}

func validateSimpleName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return errors.New("invalid file name")
	}
	return nil
}

func isTextFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, value := range []string{".txt", ".md", ".json", ".yaml", ".yml", ".go", ".js", ".css", ".html", ".csv", ".log", ".ini", ".conf"} {
		if ext == value {
			return true
		}
	}
	return false
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path query parameter is required"))
		return
	}
	var versions []localapp.FileVersion
	err := s.withApp(func(app *localapp.App) error {
		var err error
		versions, err = app.Versions(r.Context(), path)
		return err
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

type restoreRequest struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req restoreRequest
	if err := decodeJSON(r, &req); err != nil || strings.TrimSpace(req.Path) == "" || strings.TrimSpace(req.Version) == "" {
		writeError(w, http.StatusBadRequest, errors.New("path and version are required"))
		return
	}
	var version string
	err := s.withApp(func(app *localapp.App) error {
		var err error
		version, err = app.Restore(r.Context(), req.Path, req.Version)
		return err
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": version})
}

type monitorRequest struct {
	Action string `json:"action"`
}

func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req monitorRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "start":
		if err := s.startMonitorLocked(); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	case "stop":
		s.stopMonitorLocked()
	default:
		writeError(w, http.StatusBadRequest, errors.New("action must be start or stop"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"monitoring": s.monitorCancel != nil})
}

// ensureRemoteMonitor starts the background bidirectional monitor after a
// saved connection is loaded or paired. It deliberately does nothing for a
// local-only library so the existing manual local monitor remains available.
func (s *Server) ensureRemoteMonitor() {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.app == nil || s.cfg.ServerURL == "" || s.cfg.SpaceID == "" || s.cfg.DeviceID == "" || s.cfg.DeviceToken == "" {
		return
	}
	_ = s.startMonitorLocked()
}

// restartRemoteMonitor replaces a running monitor after connection settings
// change. The monitor captures its RemoteClient at startup, so leaving the
// old goroutine alive would keep using an expired token or previous device.
func (s *Server) restartRemoteMonitor() {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopMonitorLocked()
	if s.app == nil || s.cfg.ServerURL == "" || s.cfg.SpaceID == "" || s.cfg.DeviceID == "" || s.cfg.DeviceToken == "" {
		return
	}
	_ = s.startMonitorLocked()
}

// startMonitorLocked requires operations and mu to be held by the caller.
func (s *Server) startMonitorLocked() error {
	if s.app == nil {
		return errors.New("open a local library first")
	}
	if s.monitorCancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	app := s.app
	cfg := s.cfg
	s.monitorCancel, s.monitorDone = cancel, done
	go func() {
		defer close(done)
		if cfg.ServerURL != "" && cfg.SpaceID != "" && cfg.DeviceID != "" && cfg.DeviceToken != "" {
			timer := time.NewTimer(remoteMonitorStartupDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			remote := &objectstore.RemoteClient{BaseURL: cfg.ServerURL, SpaceID: cfg.SpaceID, DeviceID: cfg.DeviceID, DeviceToken: cfg.DeviceToken}
			_ = app.RunSync(ctx, remote, remoteSyncPollInterval)
			return
		}
		_ = app.Run(ctx, 2*time.Minute)
	}()
	return nil
}

func (s *Server) withApp(fn func(*localapp.App) error) error {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.RLock()
	app := s.app
	s.mu.RUnlock()
	if app == nil {
		return &httpError{status: http.StatusConflict, err: errors.New("open a local library first")}
	}
	return fn(app)
}

func (s *Server) stopMonitorLocked() {
	if s.monitorCancel == nil {
		return
	}
	s.monitorCancel()
	<-s.monitorDone
	s.monitorCancel, s.monitorDone = nil, nil
}

type httpError struct {
	status int
	err    error
}

func (e *httpError) Error() string { return e.err.Error() }

func writeAppError(w http.ResponseWriter, err error) {
	var appErr *httpError
	if errors.As(err, &appErr) {
		writeError(w, appErr.status, appErr.err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON request")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
}
