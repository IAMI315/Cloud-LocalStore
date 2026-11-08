package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/IAMI315/Cloud-LocalStore/internal/server"
)

func main() {
	configPath := flag.String("config", "", "path to server config")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	app, err := server.NewApp(cfg, logger)
	if err != nil {
		logger.Error("initialize server", "error", err)
		os.Exit(1)
	}
	defer app.Close()
	logger.Info("server initialized", "data_dir", cfg.DataDir)
}
