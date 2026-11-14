package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/server"
)

const serverVersion = "0.1.0"

func main() {
	configPath := flag.String("config", "", "path to server config")
	addr := flag.String("addr", "", "HTTP listen address (overrides config and starts server mode)")
	storageQuota := flag.Int64("storage-quota", 0, "default storage quota in bytes (overrides config)")
	once := flag.Bool("once", false, "initialize and exit (for diagnostics)")
	showPairing := flag.Bool("show-pairing", false, "print the pairing token and exit")
	menu := flag.Bool("menu", false, "open the interactive management console")
	serve := flag.Bool("serve", false, "run only the HTTP service")
	version := flag.Bool("version", false, "print the server version")
	flag.Parse()

	if *version {
		fmt.Println(serverVersion)
		return
	}
	if *menu && *serve {
		fmt.Fprintln(os.Stderr, "--menu and --serve cannot be used together")
		os.Exit(2)
	}

	baseDir := executableDir()
	cfg, resolvedConfig, err := server.LoadOrCreateConfig(*configPath, baseDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if *addr != "" {
		cfg.ListenAddr = *addr
	}
	if *storageQuota != 0 {
		cfg.StorageQuotaBytes = *storageQuota
	}
	if err := server.ValidateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid server config: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	app, err := server.NewApp(cfg, logger)
	if err != nil {
		logger.Error("initialize server", "error", err)
		os.Exit(1)
	}
	defer app.Close()

	if *showPairing {
		_, _ = fmt.Fprintln(os.Stdout, app.PairingToken())
		return
	}
	if *once {
		logger.Info("server initialized", "data_dir", cfg.DataDir, "config", resolvedConfig)
		return
	}

	// Running without an explicit service-only flag opens the management
	// console. The console controls a detached --serve child process; keeping
	// the roles separate lets option 0 return to the shell while the service
	// continues running.
	// A completely argument-free launch is the portable interactive experience.
	// An explicit config path keeps the historical service invocation working,
	// which is important for existing VPS startup scripts.
	interactive := *menu || (!*serve && *configPath == "" && *addr == "")
	runtime := newServerRuntime(app, logger, resolvedConfig)
	if interactive {
		if err := runInteractive(app, runtime, resolvedConfig); err != nil {
			logger.Error("management console stopped", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := runService(runtime); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func runService(runtime *serverRuntime) error {
	if !runtime.Running() {
		if err := runtime.Start(); err != nil {
			return err
		}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	select {
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := runtime.Stop(ctx)
		cancel()
		return err
	case err := <-runtime.Errors():
		return err
	}
}

func executableDir() string {
	path, err := os.Executable()
	if err != nil {
		return "."
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Dir(path)
	}
	dir := filepath.Dir(path)
	// The VPS layout places the binary in <root>/bin while the generated
	// configuration and data live beside bin. Treat that layout as one portable
	// application directory; other layouts continue to use the executable dir.
	if strings.EqualFold(filepath.Base(dir), "bin") {
		return filepath.Dir(dir)
	}
	return dir
}
