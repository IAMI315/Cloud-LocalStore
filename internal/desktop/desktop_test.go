package desktop

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOptionsDefaultToLoopback(t *testing.T) {
	if got := (Options{}).normalized().Addr; got != DefaultAddr {
		t.Fatalf("default address = %q, want %q", got, DefaultAddr)
	}
}

func TestStartAndShutdownOwnLoopbackService(t *testing.T) {
	instance, err := Start(Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(instance.URL(), "http://127.0.0.1:") {
		t.Fatalf("URL = %q, want loopback URL", instance.URL())
	}
	res, err := http.Get(instance.URL() + "api/state")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("state status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if err := instance.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-instance.serveErr:
		if err != nil {
			t.Fatalf("serve after shutdown = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after shutdown")
	}
	client := http.Client{Timeout: 500 * time.Millisecond}
	if _, err := client.Get(instance.URL() + "api/state"); err == nil {
		t.Fatal("request succeeded after shutdown")
	}
}

func TestRunStopsOnContextAndDoesNotOpenBrowserWhenDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &readyWriter{ready: make(chan struct{})}
	browserCalled := false
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Addr: "127.0.0.1:0", NoBrowser: true}, output, func(string) error {
			browserCalled = true
			return nil
		})
	}()
	select {
	case <-output.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not start the service")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
	if browserCalled {
		t.Fatal("browser was opened with NoBrowser")
	}
}

type readyWriter struct {
	mu    sync.Mutex
	ready chan struct{}
	once  sync.Once
}

func (w *readyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}
