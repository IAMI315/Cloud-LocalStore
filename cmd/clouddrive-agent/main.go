package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/IAMI315/Cloud-LocalStore/internal/agent"
)

func main() {
	configPath := flag.String("config", "", "path to agent config")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := agent.NewApp(cfg, logger)
	if err != nil {
		logger.Error("initialize agent", "error", err)
		os.Exit(1)
	}
	defer app.Close()
	if err := app.Run(ctx, 0); err != nil && ctx.Err() == nil {
		logger.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}
