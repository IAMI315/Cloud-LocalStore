package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/server"
)

func main() {
	configPath := flag.String("config", "", "path to server config")
	addr := flag.String("addr", "", "HTTP listen address (overrides config)")
	once := flag.Bool("once", false, "initialize and exit (for diagnostics)")
	showPairing := flag.Bool("show-pairing", false, "print the pairing token and exit")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	if *addr != "" {
		cfg.ListenAddr = *addr
	}
	if err := server.ValidateConfig(cfg); err != nil {
		logger.Error("invalid server config", "error", err)
		os.Exit(1)
	}
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
		logger.Info("server initialized", "data_dir", cfg.DataDir)
		return
	}
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		logger.Info("server listening over HTTP", "addr", cfg.ListenAddr, "tls", false, "authentication", true)
		if !server.IsLoopbackListenAddr(cfg.ListenAddr) {
			logger.Warn("non-loopback HTTP address has no TLS; use bearer authentication and restrict access or add a TLS reverse proxy", "addr", cfg.ListenAddr)
		}
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}()
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}
