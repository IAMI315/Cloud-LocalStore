package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/filesystem"
	"github.com/IAMI315/Cloud-LocalStore/internal/localapp"
	"github.com/IAMI315/Cloud-LocalStore/internal/objectstore"
	"github.com/IAMI315/Cloud-LocalStore/internal/protocol"
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
type autoStartGetter func() (bool, error)
type autoStartSetter func(bool) error

type Server struct {
	mu                sync.RWMutex
	operations        sync.Mutex
	app               *localapp.App
	cfg               localapp.Config
	monitorCancel     context.CancelFunc
	monitorDone       chan struct{}
	addr              string
	httpServer        *http.Server
	static            fs.FS
	selectFolder      folderSelector
	autoStartGet      autoStartGetter
	autoStartSet      autoStartSetter
	autoStartSupport  bool
	closing           atomic.Bool
	connectionMu      sync.Mutex
	connectionKey     string
	connection        remoteConnectionStatus
	remoteDevices     []protocol.Device
	remoteRevision    int64
	remoteServerTime  string
	remoteQuota       int64
	remoteUsed        int64
	remoteQuotaSynced bool
	remoteStateStale  bool
	probeRunning      bool
	probeKey          string
}

type remoteConnectionStatus struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	Authorized bool   `json:"authorized"`
	Status     string `json:"status"`
	CheckedAt  string `json:"checked_at,omitempty"`
	LatencyMS  int64  `json:"latency_ms,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

func New(addr string) *Server {
	if strings.TrimSpace(addr) == "" {
		addr = defaultAddr
	}
	static, _ := fs.Sub(webFiles, "web")
	s := &Server{addr: addr, static: static, selectFolder: selectFolder}
	return s
}

// SetAutoStartController connects the platform desktop host to the settings
// API without making the GUI package depend on Windows registry APIs.
func (s *Server) SetAutoStartController(get autoStartGetter, set autoStartSetter, supported bool) {
	s.mu.Lock()
	s.autoStartGet, s.autoStartSet, s.autoStartSupport = get, set, supported
	s.mu.Unlock()
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
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/library", s.handleLibrary)
	mux.HandleFunc("/api/select-folder", s.handleSelectFolder)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/files", s.handleFiles)
	mux.HandleFunc("/api/browser", s.handleBrowser)
	mux.HandleFunc("/api/conflicts", s.handleConflicts)
	mux.HandleFunc("/api/conflicts/resolve", s.handleConflictResolve)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/file", s.handleFileAction)
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/api/versions", s.handleVersions)
	mux.HandleFunc("/api/restore", s.handleRestore)
	mux.HandleFunc("/api/monitor", s.handleMonitor)
	mux.HandleFunc("/api/connection", s.handleConnection)
	mux.HandleFunc("/api/quota", s.handleQuota)
	mux.HandleFunc("/api/autostart", s.handleAutoStart)
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

// BeginShutdown marks the server as closing and stops background monitoring
// without waiting for HTTP handlers. It is safe to call from the WebView UI
// thread before the native message loop terminates.
func (s *Server) BeginShutdown() {
	if s.closing.Swap(true) {
		return
	}
	s.mu.RLock()
	cancel := s.monitorCancel
	s.mu.RUnlock()
	if cancel != nil {
		// Do not wait here: this method is called from the native WebView UI
		// thread. Shutdown performs the ordered wait after the window exits.
		cancel()
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	// Stop background reconciliation before waiting for HTTP handlers. This
	// releases SQLite/network work that state and browser requests may be
	// waiting behind during tray exit.
	s.BeginShutdown()
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
	autoStart, autoStartSupported := s.autoStartSnapshot()
	result := map[string]any{"configured": app != nil, "network": "未配置", "autostart": autoStart, "autostart_supported": autoStartSupported}
	if app == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	status, err := app.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result["root"], result["data_dir"] = cfg.RootPath, cfg.DataDir
	result["server_url"], result["space_id"], result["device_id"] = cfg.ServerURL, cfg.SpaceID, cfg.DeviceID
	connection := s.connectionSnapshot(cfg)
	result["connection"] = connection
	result["network"] = connectionNetworkLabel(connection.Status)
	result["status"] = status
	result["pending"] = status.Pending
	result["last_sync_at"] = status.LastSyncAt
	result["index_updated_at"] = status.IndexUpdatedAt
	deviceSnapshot := s.remoteDeviceSnapshot()
	result["devices"] = deviceSnapshot.Devices
	result["current_revision"] = deviceSnapshot.CurrentRevision
	result["server_time"] = deviceSnapshot.ServerTime
	result["quota_bytes"] = deviceSnapshot.QuotaBytes
	result["quota_used_bytes"] = deviceSnapshot.UsedBytes
	result["quota_synced"] = deviceSnapshot.QuotaSynced
	result["devices_stale"] = deviceSnapshot.Stale
	s.mu.RLock()
	result["monitoring"] = s.monitorCancel != nil
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) autoStartSnapshot() (bool, bool) {
	s.mu.RLock()
	get := s.autoStartGet
	supported := s.autoStartSupport
	s.mu.RUnlock()
	if get == nil || !supported {
		return false, false
	}
	enabled, err := get()
	if err != nil {
		return false, true
	}
	return enabled, true
}

func (s *Server) handleAutoStart(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	get, set := s.autoStartGet, s.autoStartSet
	supported := s.autoStartSupport
	s.mu.RUnlock()
	if get == nil || set == nil || !supported {
		writeError(w, http.StatusNotImplemented, errors.New("当前平台不支持开机自动启动"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		enabled, err := get()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "supported": true})
	case http.MethodPost:
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := set(req.Enabled); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": req.Enabled, "supported": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	connection := s.connectionSnapshot(cfg)
	snapshot := s.remoteDeviceSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"connection":       connection,
		"server_url":       cfg.ServerURL,
		"space_id":         cfg.SpaceID,
		"device_id":        cfg.DeviceID,
		"devices":          snapshot.Devices,
		"current_revision": snapshot.CurrentRevision,
		"server_time":      snapshot.ServerTime,
		"stale":            snapshot.Stale,
	})
}

type remoteDeviceSnapshot struct {
	Devices         []protocol.Device `json:"devices"`
	CurrentRevision int64             `json:"current_revision,omitempty"`
	ServerTime      string            `json:"server_time,omitempty"`
	QuotaBytes      int64             `json:"quota_bytes,omitempty"`
	UsedBytes       int64             `json:"used_bytes,omitempty"`
	QuotaSynced     bool              `json:"quota_synced"`
	Stale           bool              `json:"stale"`
}

func (s *Server) remoteDeviceSnapshot() remoteDeviceSnapshot {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	devices := append([]protocol.Device(nil), s.remoteDevices...)
	return remoteDeviceSnapshot{Devices: devices, CurrentRevision: s.remoteRevision, ServerTime: s.remoteServerTime, QuotaBytes: s.remoteQuota, UsedBytes: s.remoteUsed, QuotaSynced: s.remoteQuotaSynced, Stale: s.remoteStateStale}
}

func connectionNetworkLabel(status string) string {
	switch status {
	case "ready":
		return "已连接"
	case "checking":
		return "检测中"
	case "offline":
		return "服务器离线"
	case "unauthorized":
		return "未授权"
	case "unconfigured":
		return "未配置"
	default:
		return "连接异常"
	}
}

func (s *Server) connectionSnapshot(cfg localapp.Config) remoteConnectionStatus {
	configured := strings.TrimSpace(cfg.ServerURL) != "" && strings.TrimSpace(cfg.SpaceID) != "" && strings.TrimSpace(cfg.DeviceID) != "" && strings.TrimSpace(cfg.DeviceToken) != ""
	if !configured {
		return remoteConnectionStatus{Configured: false, Status: "unconfigured"}
	}
	key := strings.Join([]string{cfg.ServerURL, cfg.SpaceID, cfg.DeviceID, cfg.DeviceToken}, "\x00")
	now := time.Now()
	s.connectionMu.Lock()
	if s.connectionKey != key {
		s.connectionKey = key
		s.connection = remoteConnectionStatus{Configured: true, Status: "checking"}
		s.remoteDevices = nil
		s.remoteRevision = 0
		s.remoteServerTime = ""
		s.remoteQuota = 0
		s.remoteUsed = 0
		s.remoteQuotaSynced = false
		s.remoteStateStale = true
		s.probeRunning = false
		s.probeKey = ""
	}
	if s.connection.Status == "" {
		s.connection = remoteConnectionStatus{Configured: true, Status: "checking"}
	}
	snapshot := s.connection
	checkedAt, _ := time.Parse(time.RFC3339Nano, snapshot.CheckedAt)
	if !s.probeRunning && (snapshot.CheckedAt == "" || now.Sub(checkedAt) >= 3*time.Second) {
		s.probeRunning = true
		s.probeKey = key
		go s.probeConnection(cfg, key)
	}
	s.connectionMu.Unlock()
	return snapshot
}

func (s *Server) probeConnection(cfg localapp.Config, key string) {
	started := time.Now()
	result := remoteConnectionStatus{Configured: true, Status: "offline"}
	parsed, err := url.Parse(strings.TrimSpace(cfg.ServerURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		result.Status = "error"
		result.ErrorCode, result.Error = "invalid_url", "服务地址无效"
	} else {
		client := &http.Client{Timeout: 2500 * time.Millisecond}
		healthReq, reqErr := http.NewRequest(http.MethodGet, strings.TrimRight(cfg.ServerURL, "/")+"/healthz", nil)
		if reqErr != nil {
			result.ErrorCode, result.Error = "invalid_url", "服务地址无效"
		} else if resp, healthErr := client.Do(healthReq); healthErr != nil {
			result.ErrorCode, result.Error = "network_error", "服务器无法连接"
		} else {
			_ = resp.Body.Close()
			result.Reachable = true
			if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
				result.Status = "error"
				result.ErrorCode, result.Error = "server_error", fmt.Sprintf("服务器返回 HTTP %d", resp.StatusCode)
			} else {
				stateURL := strings.TrimRight(cfg.ServerURL, "/") + "/v1/spaces/" + url.PathEscape(cfg.SpaceID) + "/state"
				stateReq, stateReqErr := http.NewRequest(http.MethodGet, stateURL, nil)
				if stateReqErr != nil {
					result.Status = "error"
					result.ErrorCode, result.Error = "invalid_url", "服务地址无效"
				} else {
					stateReq.Header.Set("X-Device-ID", cfg.DeviceID)
					stateReq.Header.Set("X-Space-ID", cfg.SpaceID)
					stateReq.Header.Set("Authorization", "Bearer "+cfg.DeviceToken)
					stateResp, stateErr := client.Do(stateReq)
					if stateErr != nil {
						result.Status = "offline"
						result.ErrorCode, result.Error = "network_error", "服务器连接超时"
					} else {
						var workspace protocol.WorkspaceState
						decodeErr := json.NewDecoder(stateResp.Body).Decode(&workspace)
						_ = stateResp.Body.Close()
						switch {
						case stateResp.StatusCode == http.StatusUnauthorized || stateResp.StatusCode == http.StatusForbidden:
							result.Status, result.ErrorCode, result.Error = "unauthorized", "unauthorized", "设备未授权或令牌已失效"
						case stateResp.StatusCode < http.StatusOK || stateResp.StatusCode >= http.StatusMultipleChoices:
							result.Status = "error"
							result.ErrorCode, result.Error = "server_error", fmt.Sprintf("同步接口返回 HTTP %d", stateResp.StatusCode)
						case decodeErr != nil:
							result.Status = "error"
							result.ErrorCode, result.Error = "invalid_response", "服务器状态响应无效"
						default:
							result.Status, result.Authorized = "ready", true
							s.connectionMu.Lock()
							if s.connectionKey == key {
								s.remoteDevices = append([]protocol.Device(nil), workspace.Devices...)
								s.remoteRevision = workspace.CurrentRevision
								s.remoteServerTime = workspace.ServerTime
								s.remoteQuota = workspace.QuotaBytes
								s.remoteUsed = workspace.UsedBytes
								s.remoteQuotaSynced = workspace.QuotaBytes > 0
								s.remoteStateStale = false
							}
							s.connectionMu.Unlock()
						}
					}
				}
			}
		}
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	result.CheckedAt = time.Now().Format(time.RFC3339Nano)
	s.connectionMu.Lock()
	if s.connectionKey == key && s.probeKey == key {
		s.connection = result
		if result.Status != "ready" {
			s.remoteStateStale = true
		}
		s.probeRunning = false
		s.probeKey = ""
	} else if s.probeKey == key {
		s.probeRunning = false
		s.probeKey = ""
	}
	s.connectionMu.Unlock()
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
			if previousDeviceID != "" && previousDeviceID != cfg.DeviceID && pairingProvided {
				// The legacy settings field was also used as the display name. Keep
				// the old stable identity when the user changes that field.
				remote.PreviousDeviceID = previousDeviceID
			}
			paired, err := remote.RegisterDevice(r.Context(), cfg.DeviceID, cfg.PairingToken)
			if err != nil {
				return fmt.Errorf("pair device: %w", err)
			}
			cfg.DeviceToken = paired.DeviceToken
			if cfg.DeviceToken == "" {
				return errors.New("server returned an empty device token")
			}
			if paired.Device.DeviceID != "" {
				cfg.DeviceID = paired.Device.DeviceID
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

func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	if cfg.ServerURL == "" || cfg.SpaceID == "" || cfg.DeviceID == "" || cfg.DeviceToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("服务端连接未配置"))
		return
	}
	remote := &objectstore.RemoteClient{BaseURL: cfg.ServerURL, SpaceID: cfg.SpaceID, DeviceID: cfg.DeviceID, DeviceToken: cfg.DeviceToken}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var response protocol.SetQuotaResponse
	var err error
	if r.Method == http.MethodGet {
		state, stateErr := remote.WorkspaceState(ctx)
		if stateErr != nil {
			err = stateErr
		} else {
			response = protocol.SetQuotaResponse{SpaceID: state.SpaceID, QuotaBytes: state.QuotaBytes, UsedBytes: state.UsedBytes}
		}
	} else {
		var req protocol.SetQuotaRequest
		if err = decodeJSON(r, &req); err == nil {
			err = protocol.ValidateSetQuotaRequest(req)
		}
		if err == nil {
			response, err = remote.SetQuota(ctx, req.QuotaBytes)
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "remote quota_exceeded") {
			writeError(w, http.StatusInsufficientStorage, errors.New("空间容量不足，服务端拒绝保存"))
			return
		}
		if strings.Contains(err.Error(), "remote method_not_allowed") {
			writeError(w, http.StatusConflict, errors.New("当前服务端版本不支持空间容量设置，请更新并重启服务端"))
			return
		}
		writeAppError(w, err)
		return
	}
	s.connectionMu.Lock()
	if s.connectionKey == strings.Join([]string{cfg.ServerURL, cfg.SpaceID, cfg.DeviceID, cfg.DeviceToken}, "\x00") {
		s.remoteQuota, s.remoteUsed, s.remoteQuotaSynced = response.QuotaBytes, response.UsedBytes, true
	}
	s.connectionMu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var presenceCfg localapp.Config
	err := s.withApp(func(app *localapp.App) error {
		s.mu.RLock()
		cfg := s.cfg
		presenceCfg = cfg
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
		if presenceCfg.ServerURL != "" {
			go s.sendPresence(presenceCfg, protocol.PresenceRequest{ClientStatus: "error", Monitoring: true})
		}
		writeAppError(w, err)
		return
	}
	if presenceCfg.ServerURL != "" {
		s.mu.RLock()
		monitoringNow := s.monitorCancel != nil
		s.mu.RUnlock()
		go s.sendPresence(presenceCfg, protocol.PresenceRequest{ClientStatus: "running", Monitoring: monitoringNow, SyncCompleted: true})
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
	Status    string `json:"status,omitempty"`
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
			conflictName := isConflictName(name)
			status := ""
			if !entry.IsDir() {
				status, err = app.FileStatus(r.Context(), entryPath, entryInfo.Size(), entryInfo.ModTime().UnixNano(), conflictName)
				if err != nil {
					return err
				}
			}
			result.Entries = append(result.Entries, browserEntry{Name: name, Path: entryPath, Directory: entry.IsDir(), Size: entryInfo.Size(), Modified: entryInfo.ModTime().Format(time.RFC3339), Kind: kind, Conflict: conflictName, Status: status})
		}
		field := strings.ToLower(r.URL.Query().Get("sort"))
		desc := strings.EqualFold(r.URL.Query().Get("order"), "desc")
		sort.SliceStable(result.Entries, func(i, j int) bool {
			a, b := result.Entries[i], result.Entries[j]
			if a.Directory != b.Directory {
				return a.Directory
			}
			compare := 0
			switch field {
			case "size":
				switch {
				case a.Size < b.Size:
					compare = -1
				case a.Size > b.Size:
					compare = 1
				}
			case "modified":
				compare = strings.Compare(a.Modified, b.Modified)
			default:
				compare = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
			}
			if compare == 0 {
				compare = strings.Compare(a.Name, b.Name)
			}
			if desc {
				return compare > 0
			}
			return compare < 0
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

type fileProperties struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size"`
	Modified  string `json:"modified"`
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var uploaded []string
	err := s.withApp(func(app *localapp.App) error {
		pathValue := strings.TrimSpace(r.URL.Query().Get("path"))
		_, targetDir, err := safeLocalPath(app.Root, pathValue, true)
		if err != nil {
			return err
		}
		info, err := os.Stat(targetDir)
		if err != nil || !info.IsDir() {
			return errors.New("target must be a directory")
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<30)
		reader, err := r.MultipartReader()
		if err != nil {
			return errors.New("multipart upload is required")
		}
		for {
			part, partErr := reader.NextPart()
			if errors.Is(partErr, io.EOF) {
				break
			}
			if partErr != nil {
				return partErr
			}
			name := strings.TrimSpace(part.FileName())
			if name == "" {
				_ = part.Close()
				continue
			}
			name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
			if err := validateSimpleName(name); err != nil {
				_ = part.Close()
				return err
			}
			destination := filepath.Join(targetDir, uniqueFileName(targetDir, name, false))
			temporary, err := os.CreateTemp(targetDir, ".clouddrive-drop-*")
			if err != nil {
				_ = part.Close()
				return err
			}
			temporaryName := temporary.Name()
			_, copyErr := io.Copy(temporary, part)
			closeErr := temporary.Close()
			_ = part.Close()
			if copyErr != nil || closeErr != nil {
				_ = os.Remove(temporaryName)
				if copyErr != nil {
					return copyErr
				}
				return closeErr
			}
			if err := os.Chmod(temporaryName, 0o600); err != nil {
				_ = os.Remove(temporaryName)
				return err
			}
			if err := os.Rename(temporaryName, destination); err != nil {
				_ = os.Remove(temporaryName)
				return err
			}
			uploaded = append(uploaded, filepath.ToSlash(filepath.Join(pathValue, filepath.Base(destination))))
		}
		return nil
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploaded": uploaded})
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
	var properties *fileProperties
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
		case "copy", "move":
			if len(req.Paths) == 0 {
				return errors.New("paths are required")
			}
			_, targetDir, err := safeLocalPath(app.Root, req.Target, true)
			if err != nil {
				return err
			}
			if info, statErr := os.Stat(targetDir); statErr != nil || !info.IsDir() {
				return errors.New("target must be a directory")
			}
			for _, candidate := range req.Paths {
				_, source, pathErr := safeLocalPath(app.Root, candidate, false)
				if pathErr != nil {
					return pathErr
				}
				info, statErr := os.Stat(source)
				if statErr != nil {
					return statErr
				}
				if info.IsDir() {
					rel, relErr := filepath.Rel(source, targetDir)
					if relErr != nil {
						return relErr
					}
					if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
						return errors.New("target cannot be inside source directory")
					}
				}
				name := uniqueFileName(targetDir, filepath.Base(source), info.IsDir())
				destination := filepath.Join(targetDir, name)
				if action == "move" {
					if err := os.Rename(source, destination); err != nil {
						return err
					}
					continue
				}
				if err := copyLocalEntry(source, destination, info.IsDir()); err != nil {
					return err
				}
			}
			return nil
		case "properties":
			_, target, err := safeLocalPath(app.Root, path, false)
			if err != nil {
				return err
			}
			info, err := os.Stat(target)
			if err != nil {
				return err
			}
			props := fileProperties{Name: info.Name(), Path: filepath.ToSlash(path), Directory: info.IsDir(), Size: info.Size(), Modified: info.ModTime().Format(time.RFC3339)}
			properties = &props
			return nil
		default:
			return errors.New("action must be mkdir, create, rename, delete, copy, move or properties")
		}
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	if properties != nil {
		writeJSON(w, http.StatusOK, properties)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func uniqueFileName(directory, name string, isDirectory bool) string {
	candidate := name
	if _, err := os.Lstat(filepath.Join(directory, candidate)); errors.Is(err, os.ErrNotExist) {
		return candidate
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		suffix := fmt.Sprintf(" - 副本 (%d)", i)
		if isDirectory {
			candidate = name + suffix
		} else {
			candidate = base + suffix + ext
		}
		if _, err := os.Lstat(filepath.Join(directory, candidate)); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func copyLocalEntry(source, destination string, directory bool) error {
	if directory {
		return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			target := filepath.Join(destination, rel)
			if info.IsDir() {
				return os.MkdirAll(target, info.Mode().Perm())
			}
			return copyLocalFile(path, target, info.Mode().Perm())
		})
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	return copyLocalFile(source, destination, info.Mode().Perm())
}

func copyLocalFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
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
	s.mu.Lock()
	var action string
	var cfg localapp.Config
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "start":
		if err := s.startMonitorLocked(); err != nil {
			s.mu.Unlock()
			s.operations.Unlock()
			writeError(w, http.StatusConflict, err)
			return
		}
		action = "running"
	case "stop":
		s.stopMonitorLocked()
		action = "paused"
	default:
		s.mu.Unlock()
		s.operations.Unlock()
		writeError(w, http.StatusBadRequest, errors.New("action must be start or stop"))
		return
	}
	monitoringNow := s.monitorCancel != nil
	cfg = s.cfg
	s.mu.Unlock()
	s.operations.Unlock()
	if cfg.ServerURL != "" && cfg.SpaceID != "" && cfg.DeviceID != "" && cfg.DeviceToken != "" {
		go s.sendPresence(cfg, protocol.PresenceRequest{ClientStatus: action, Monitoring: monitoringNow})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"monitoring": monitoringNow})
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
			events := &objectstore.RemoteClient{BaseURL: cfg.ServerURL, SpaceID: cfg.SpaceID, DeviceID: cfg.DeviceID, DeviceToken: cfg.DeviceToken, HTTP: &http.Client{}}
			var workers sync.WaitGroup
			workers.Add(3)
			go func() { defer workers.Done(); _ = app.RunSync(ctx, remote, remoteSyncPollInterval) }()
			go func() { defer workers.Done(); s.remotePresenceLoop(ctx, remote) }()
			go func() { defer workers.Done(); s.remoteEventLoop(ctx, events) }()
			workers.Wait()
			return
		}
		_ = app.Run(ctx, 2*time.Minute)
	}()
	return nil
}

func (s *Server) sendPresence(cfg localapp.Config, presence protocol.PresenceRequest) {
	if cfg.ServerURL == "" || cfg.SpaceID == "" || cfg.DeviceID == "" || cfg.DeviceToken == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	remote := &objectstore.RemoteClient{BaseURL: cfg.ServerURL, SpaceID: cfg.SpaceID, DeviceID: cfg.DeviceID, DeviceToken: cfg.DeviceToken}
	response, err := remote.SendPresence(ctx, presence)
	if err != nil {
		return
	}
	s.updateRemoteDevice(response.Device)
}

func (s *Server) remotePresenceLoop(ctx context.Context, remote *objectstore.RemoteClient) {
	presence := protocol.PresenceRequest{ClientStatus: "running", Monitoring: true}
	if response, err := remote.SendPresence(ctx, presence); err == nil {
		s.updateRemoteDevice(response.Device)
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if response, err := remote.SendPresence(ctx, presence); err == nil {
				s.updateRemoteDevice(response.Device)
			}
		}
	}
}

func (s *Server) remoteEventLoop(ctx context.Context, remote *objectstore.RemoteClient) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		response, err := remote.OpenEvents(ctx)
		if err == nil {
			backoff = time.Second
			err = s.consumeRemoteEvents(ctx, response.Body)
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		_ = err
	}
}

func (s *Server) consumeRemoteEvents(ctx context.Context, body io.Reader) error {
	reader := bufio.NewReader(body)
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			var event protocol.DeviceEvent
			if json.Unmarshal([]byte(data.String()), &event) == nil && event.Device.DeviceID != "" {
				s.updateRemoteDevice(event.Device)
			}
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (s *Server) updateRemoteDevice(device protocol.Device) {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	found := false
	for i := range s.remoteDevices {
		if s.remoteDevices[i].DeviceID == device.DeviceID {
			s.remoteDevices[i] = device
			found = true
			break
		}
	}
	if !found {
		s.remoteDevices = append(s.remoteDevices, device)
	}
	s.remoteStateStale = false
	s.connection.Reachable = true
	s.connection.Authorized = true
	s.connection.Status = "ready"
	s.connection.CheckedAt = time.Now().Format(time.RFC3339Nano)
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
