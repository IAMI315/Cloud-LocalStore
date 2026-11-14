package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/server"
)

type serverRuntime struct {
	app        *server.App
	logger     *slog.Logger
	cfg        server.Config
	configPath string

	mu          sync.Mutex
	listener    net.Listener
	http        *http.Server
	startedAt   time.Time
	external    bool
	externalPID int
	errCh       chan error
}

var errExternalServer = errors.New("server is already running in another process")

func newServerRuntime(app *server.App, logger *slog.Logger, configPath string) *serverRuntime {
	return &serverRuntime{app: app, logger: logger, cfg: app.Config(), configPath: configPath, errCh: make(chan error, 1)}
}

// StartBackground starts the service role of this same binary as a detached
// child. The manager remains interactive, while the child owns the listener
// and survives when the manager exits back to the shell.
func (r *serverRuntime) StartBackground(configPath string) error {
	r.mu.Lock()
	if r.listener != nil || r.external {
		r.mu.Unlock()
		return nil
	}
	if configPath == "" {
		configPath = r.configPath
	}
	r.mu.Unlock()
	if configPath == "" {
		return errors.New("server config path is required")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate server executable: %w", err)
	}
	if executable, err = filepath.EvalSymlinks(executable); err == nil {
		// Keep the resolved path so upgrades through symlinks do not spawn a
		// process from an unexpected working directory.
	} else {
		executable = filepath.Clean(executable)
	}
	logPath := filepath.Join(r.cfg.DataDir, "server.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return fmt.Errorf("create server log directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open server log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open null input: %w", err)
	}
	defer func() { _ = devNull.Close() }()
	cmd := exec.Command(executable, "--serve", "--config", configPath)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start background service: %w", err)
	}
	pid := cmd.Process.Pid
	if err := r.waitForHealth(5 * time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("background service failed to become ready: %w", err)
	}
	r.mu.Lock()
	r.external, r.externalPID, r.startedAt = true, pid, time.Now()
	r.mu.Unlock()
	return nil
}

func (r *serverRuntime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.external {
		return errExternalServer
	}
	if r.listener != nil {
		return nil
	}
	listener, err := net.Listen("tcp", r.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", r.cfg.ListenAddr, err)
	}
	h := &http.Server{
		Addr:              r.cfg.ListenAddr,
		Handler:           r.app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// SSE device events are long-lived; a finite WriteTimeout would close
		// every event stream before its 20-second keepalive can be delivered.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}
	r.listener, r.http, r.startedAt = listener, h, time.Now()
	if err := r.writePID(os.Getpid()); err != nil {
		_ = listener.Close()
		r.listener, r.http = nil, nil
		return fmt.Errorf("write server pid: %w", err)
	}
	go func() {
		r.logger.Info("server listening over HTTP", "addr", r.cfg.ListenAddr, "tls", false, "authentication", true)
		if !server.IsLoopbackListenAddr(r.cfg.ListenAddr) {
			r.logger.Warn("non-loopback HTTP address has no TLS; use bearer authentication and restrict access or add a TLS reverse proxy", "addr", r.cfg.ListenAddr)
		}
		if err := h.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case r.errCh <- err:
			default:
			}
		}
	}()
	return nil
}

// AttachIfHealthy detects a service started by another process. It lets the
// interactive console inspect the same database without attempting a second
// listener on the configured port.
func (r *serverRuntime) AttachIfHealthy() bool {
	r.mu.Lock()
	if r.listener != nil || r.external {
		r.mu.Unlock()
		return r.external
	}
	addr := r.cfg.ListenAddr
	r.mu.Unlock()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return false
	}
	pid := r.readPID()
	if pid <= 0 {
		return false
	}
	r.mu.Lock()
	r.external = true
	r.externalPID = pid
	r.startedAt = time.Now()
	r.mu.Unlock()
	return true
}

func (r *serverRuntime) Refresh() {
	r.mu.Lock()
	external := r.external
	pid := r.externalPID
	r.mu.Unlock()
	if external && !r.probeHealth() {
		r.mu.Lock()
		r.external, r.externalPID = false, 0
		r.mu.Unlock()
		if pid > 0 {
			r.removePID(pid)
		}
	}
}

func (r *serverRuntime) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.external {
		pid := r.externalPID
		r.mu.Unlock()
		if pid <= 0 {
			return errExternalServer
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("find external server process: %w", err)
		}
		if err := process.Signal(os.Interrupt); err != nil {
			if runtime.GOOS != "windows" {
				return fmt.Errorf("stop external server process: %w", err)
			}
			if killErr := process.Kill(); killErr != nil {
				return fmt.Errorf("stop external server process: %w; kill fallback: %v", err, killErr)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if !r.probeHealth() {
				r.mu.Lock()
				r.external, r.externalPID = false, 0
				r.mu.Unlock()
				r.removePID(pid)
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		return fmt.Errorf("external server process did not stop")
	}
	h := r.http
	r.http, r.listener = nil, nil
	r.mu.Unlock()
	if h == nil {
		return nil
	}
	if err := h.Shutdown(ctx); err != nil {
		return err
	}
	r.removePID(os.Getpid())
	return nil
}

func (r *serverRuntime) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listener != nil || r.external
}

func (r *serverRuntime) External() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.external
}

func (r *serverRuntime) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt
}

func (r *serverRuntime) Errors() <-chan error { return r.errCh }

func (r *serverRuntime) pidFile() string { return filepath.Join(r.cfg.DataDir, "server.pid") }

func (r *serverRuntime) writePID(pid int) error {
	return os.WriteFile(r.pidFile(), []byte(fmt.Sprintf("%d\n", pid)), 0o640)
}

func (r *serverRuntime) readPID() int {
	b, err := os.ReadFile(r.pidFile())
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func (r *serverRuntime) removePID(pid int) {
	if r.readPID() == pid {
		_ = os.Remove(r.pidFile())
	}
}

func (r *serverRuntime) probeHealth() bool {
	host, port, err := net.SplitHostPort(r.cfg.ListenAddr)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
}

func (r *serverRuntime) waitForHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.probeHealth() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("health check timed out for %s", r.cfg.ListenAddr)
}
