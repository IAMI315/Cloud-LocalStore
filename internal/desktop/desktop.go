// Package desktop owns the lifecycle of the standalone local desktop entry.
// It intentionally uses the embedded HTTP console instead of a native WebView
// so the Windows binary remains CGO-free and easy to build on a clean machine.
package desktop

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/IAMI315/Cloud-LocalStore/internal/gui"
)

const DefaultAddr = "127.0.0.1:8765"

type Options struct {
	Addr      string
	NoBrowser bool
}

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

// Run starts the service, optionally opens its URL in a browser, and blocks
// until ctx is canceled or the HTTP server exits.
func Run(ctx context.Context, options Options, out io.Writer, openBrowser func(string) error) error {
	options = options.normalized()
	instance, err := Start(options)
	if err != nil {
		return err
	}
	defer func() { _ = instance.Shutdown(context.Background()) }()

	url := instance.URL()
	if _, err := fmt.Fprintf(out, "Local console listening at %s\n", url); err != nil {
		return err
	}
	if !options.NoBrowser && openBrowser != nil {
		// The browser is a convenience. A failed launch must not leak the local
		// server or make the standalone executable unusable in headless setups.
		_ = openBrowser(url)
	}
	select {
	case <-ctx.Done():
		return instance.Shutdown(context.Background())
	case err := <-instance.serveErr:
		return err
	}
}
