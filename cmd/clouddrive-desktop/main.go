// clouddrive-desktop is the double-click entry point for the local console.
// Build it with -ldflags "-H=windowsgui" to hide the console window.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/IAMI315/Cloud-LocalStore/internal/desktop"
)

func main() {
	logFile, err := openLog()
	if err != nil {
		os.Exit(1)
	}
	defer logFile.Close()
	if err := run(os.Args[1:], logFile); err != nil {
		_, _ = fmt.Fprintf(logFile, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	options, err := parseOptions(args, out)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return desktop.Run(ctx, options, out)
}

func parseOptions(args []string, out io.Writer) (desktop.Options, error) {
	fs := flag.NewFlagSet("clouddrive-desktop", flag.ContinueOnError)
	fs.SetOutput(out)
	addr := fs.String("addr", desktop.DefaultAddr, "local HTTP listen address (loopback only)")
	if err := fs.Parse(args); err != nil {
		return desktop.Options{}, err
	}
	if fs.NArg() != 0 {
		return desktop.Options{}, errors.New("usage: clouddrive-desktop [--addr 127.0.0.1:8765]")
	}
	return desktop.Options{Addr: *addr}, nil
}

func openLog() (*os.File, error) {
	exe, err := os.Executable()
	if err != nil {
		return os.OpenFile("clouddrive-desktop.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	path := filepath.Join(filepath.Dir(exe), "clouddrive-desktop.log")
	logFile, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if openErr == nil {
		return logFile, nil
	}
	// A read-only install directory should still leave a diagnostic trail in
	// the launch directory instead of failing without visible feedback.
	return os.OpenFile("clouddrive-desktop.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
