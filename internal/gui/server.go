package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/localapp"
)

const defaultAddr = "127.0.0.1:8765"

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
}

func New(addr string) *Server {
	if strings.TrimSpace(addr) == "" {
		addr = defaultAddr
	}
	static, _ := fs.Sub(webFiles, "web")
	s := &Server{addr: addr, static: static}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/library", s.handleLibrary)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/files", s.handleFiles)
	mux.HandleFunc("/api/versions", s.handleVersions)
	mux.HandleFunc("/api/restore", s.handleRestore)
	mux.HandleFunc("/api/monitor", s.handleMonitor)
	mux.Handle("/", http.FileServer(http.FS(s.static)))
	return mux
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

func (s *Server) Close() error {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopMonitorLocked()
	if s.app == nil {
		return nil
	}
	err := s.app.Close()
	s.app = nil
	s.cfg = localapp.Config{}
	return err
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
	result := map[string]any{"configured": app != nil, "network": "not connected (local prototype)"}
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
	result["status"], result["files"] = status, files
	s.mu.RLock()
	result["monitoring"] = s.monitorCancel != nil
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, result)
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
		if s.app == nil {
			writeError(w, http.StatusConflict, errors.New("open a local library first"))
			return
		}
		if s.monitorCancel == nil {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			app := s.app
			s.monitorCancel, s.monitorDone = cancel, done
			go func() {
				defer close(done)
				_ = app.Run(ctx, 2*time.Minute)
			}()
		}
	case "stop":
		s.stopMonitorLocked()
	default:
		writeError(w, http.StatusBadRequest, errors.New("action must be start or stop"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"monitoring": s.monitorCancel != nil})
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
