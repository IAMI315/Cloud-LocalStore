// Package desktop owns the lifecycle of the standalone local desktop entry.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/gui"
)

const DefaultAddr = "127.0.0.1:8765"

// ErrAlreadyRunning is returned when the tray resident desktop instance is
// already active. The second launch exits quietly instead of opening a modal
// dialog behind the existing WebView window.
var ErrAlreadyRunning = errors.New("Cloud LocalStore is already running")

type Options struct {
	Addr string
}

func (i *Instance) ServeErrors() <-chan error { return i.serveErr }

func (o Options) normalized() Options {
	if o.Addr == "" {
		o.Addr = DefaultAddr
	}
	return o
}

// Instance is a running local console. The HTTP server owns the local library,
// monitor and database resources, and Shutdown closes all of them together.
type Instance struct {
	server   *gui.Server
	listener net.Listener
	serveErr chan error
	once     sync.Once
	shutdown chan struct{}
}

func Start(options Options) (*Instance, error) {
	options = options.normalized()
	server := gui.New(options.Addr)
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		if err := server.OpenDefault(filepath.Join(base, "CloudLocalStore-Sync"), filepath.Join(base, "CloudLocalStore-State")); err != nil {
			return nil, err
		}
		server.StartConfiguredMonitor()
	}
	listener, err := server.Listen()
	if err != nil {
		return nil, err
	}
	instance := &Instance{
		server:   server,
		listener: listener,
		serveErr: make(chan error, 1),
		shutdown: make(chan struct{}),
	}
	go func() {
		instance.serveErr <- server.Serve(listener)
	}()
	return instance, nil
}

func (i *Instance) URL() string { return i.server.URL() }

func (i *Instance) Wait() error { return <-i.serveErr }

func (i *Instance) Shutdown(ctx context.Context) error {
	var err error
	i.once.Do(func() {
		close(i.shutdown)
		err = i.server.Shutdown(ctx)
	})
	return err
}

// Run starts the service and native window, then shuts the service down when
// the window closes or the service exits.
func Run(ctx context.Context, options Options, out io.Writer) error {
	options = options.normalized()
	releaseInstance, err := acquireSingleInstance()
	if err != nil {
		if !errors.Is(err, ErrAlreadyRunning) {
			showWindowError(err)
		}
		return err
	}
	defer releaseInstance()
	instance, err := Start(options)
	if err != nil {
		showWindowError(err)
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = instance.Shutdown(shutdownCtx)
	}()

	url := instance.URL()
	if _, err := fmt.Fprintf(out, "Local console listening at %s\n", url); err != nil {
		return err
	}
	// WebView2 owns a Windows message loop and must be created and run on the
	// process UI thread. The local HTTP service runs in its own goroutine.
	if err := showWindow(url); err != nil {
		showWindowError(err)
		return err
	}
	return nil
}
