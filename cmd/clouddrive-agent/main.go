package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/localapp"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut *os.File) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printUsage(out)
		return nil
	}
	command := args[0]
	args = args[1:]
	var err error
	args, err = normalizeArgs(args)
	if err != nil {
		return err
	}
	if command == "init" {
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		fs.SetOutput(errOut)
		dataDir := fs.String("data-dir", "", "state and object directory (default: <root>/.clouddrive)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: clouddrive init <directory> [--data-dir <path>]")
		}
		cfg, err := localapp.Init(fs.Arg(0), *dataDir)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Initialized local cloud drive\n  Sync directory: %s\n  Data directory: %s\n\nNext: clouddrive scan %s\n", cfg.RootPath, cfg.DataDir, quote(cfg.RootPath))
		return nil
	}
	if command != "scan" && command != "status" && command != "versions" && command != "restore" && command != "run" {
		printUsage(errOut)
		return fmt.Errorf("unknown command %q", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	dataDir := fs.String("data-dir", "", "state directory (default: <root>/.clouddrive)")
	interval := fs.Duration("interval", 2*time.Minute, "full rescan interval for run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	need := 1
	if command == "versions" {
		need = 2
	}
	if command == "restore" {
		need = 3
	}
	if fs.NArg() != need {
		return fmt.Errorf("usage: clouddrive %s <directory>%s", command, commandArgs(command))
	}
	root, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
	}
	selectedData := *dataDir
	if selectedData == "" {
		selectedData = filepath.Join(root, ".clouddrive")
	}
	selectedData, err = filepath.Abs(selectedData)
	if err != nil {
		return err
	}
	cfg, err := localapp.LoadConfig(root)
	if *dataDir != "" {
		cfg, err = localapp.LoadConfigFromData(root, selectedData)
	}
	if err != nil {
		return err
	}
	app, err := localapp.Open(cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	ctx := context.Background()
	switch command {
	case "scan":
		res, err := app.Scan(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Scan complete: %d files, %d new versions, %d deleted\n", res.Files, res.NewVersions, res.Tombstones)
	case "status":
		s, err := app.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Local cloud drive\n  Sync directory: %s\n  Data directory: %s\n  Active files: %d\n  File versions: %d\n  Deleted paths: %d\n  Network: not connected (local prototype)\n", s.Root, s.DataDir, s.ActiveFiles, s.Versions, s.Tombstones)
	case "versions":
		vs, err := app.Versions(ctx, fs.Arg(1))
		if err != nil {
			return err
		}
		if len(vs) == 0 {
			fmt.Fprintln(out, "No versions. Run `clouddrive scan` first.")
			return nil
		}
		fmt.Fprintf(out, "Versions for %s (newest last):\n", fs.Arg(1))
		for _, v := range vs {
			fmt.Fprintf(out, "  %s  %s  manifest=%s\n", v.ID, v.CreatedAt, v.Manifest)
		}
	case "restore":
		id, err := app.Restore(ctx, fs.Arg(1), fs.Arg(2))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Restored %s as new current version %s\n", fs.Arg(1), id)
	case "run":
		runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		fmt.Fprintf(out, "Watching %s (Ctrl+C to stop)\n", root)
		err := app.Run(runCtx, *interval)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

// normalizeArgs makes the CLI accept flags before or after positional
// arguments. The standard flag package stops parsing at the first positional
// argument, while the documented commands allow both forms.
func normalizeArgs(args []string) ([]string, error) {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--data-dir" || arg == "-data-dir" || arg == "--interval" || arg == "-interval" {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag %s requires a value", arg)
			}
			flags = append(flags, arg, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, "--data-dir=") || strings.HasPrefix(arg, "-data-dir=") || strings.HasPrefix(arg, "--interval=") || strings.HasPrefix(arg, "-interval=") {
			flags = append(flags, arg)
			continue
		}
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...), nil
}

func commandArgs(command string) string {
	switch command {
	case "versions":
		return " <relative-file-path> [--data-dir <path>]"
	case "restore":
		return " <relative-file-path> <version-id> [--data-dir <path>]"
	case "run":
		return " [--data-dir <path>] [--interval 2m]"
	default:
		return " [--data-dir <path>]"
	}
}
func quote(s string) string { return fmt.Sprintf("%q", s) }
func printUsage(out *os.File) {
	fmt.Fprintln(out, `clouddrive - local-first file version prototype

Usage:
  clouddrive init <directory> [--data-dir <path>]
  clouddrive scan <directory> [--data-dir <path>]
  clouddrive status <directory> [--data-dir <path>]
  clouddrive versions <directory> <relative-file-path> [--data-dir <path>]
  clouddrive restore <directory> <relative-file-path> <version-id> [--data-dir <path>]
  clouddrive run <directory> [--data-dir <path>] [--interval 2m]

By default all local state is stored in <directory>/.clouddrive.
The current prototype operates locally and does not connect to a VPS.`)
}
