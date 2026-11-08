package filesystem

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// WatchEvent is a normalized file system notification. Notifications only
// trigger a rescan; the scanner remains the source of truth.
type WatchEvent struct {
	Path string
	Op   uint32
}

type Watcher struct {
	root string
	impl *fsnotify.Watcher
	done chan struct{}
	once sync.Once
}

func NewWatcher(root string) (*Watcher, error) {
	impl, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create filesystem watcher: %w", err)
	}
	w := &Watcher{root: root, impl: impl, done: make(chan struct{})}
	// Add directories separately so a newly created directory can be added
	// when its Create event arrives.
	if err := addDirectories(impl, root); err != nil {
		_ = impl.Close()
		return nil, err
	}
	return w, nil
}

func addDirectories(w *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return w.Add(path)
		}
		return nil
	})
}

func (w *Watcher) Events() <-chan WatchEvent {
	out := make(chan WatchEvent)
	go func() {
		defer close(out)
		for event := range w.impl.Events {
			select {
			case out <- WatchEvent{Path: event.Name, Op: uint32(event.Op)}:
			case <-w.done:
				return
			}
			if event.Op&fsnotify.Create != 0 {
				_ = addDirectories(w.impl, event.Name)
			}
		}
	}()
	return out
}

func (w *Watcher) Errors() <-chan error { return w.impl.Errors }
func (w *Watcher) Close() error {
	w.once.Do(func() { close(w.done) })
	return w.impl.Close()
}

// EventsContext is useful to callers that want to stop consuming events
// without waiting for the underlying fsnotify watcher to be closed.
func (w *Watcher) EventsContext(ctx context.Context) <-chan WatchEvent {
	out := make(chan WatchEvent)
	go func() {
		defer close(out)
		for event := range w.impl.Events {
			select {
			case out <- WatchEvent{Path: event.Name, Op: uint32(event.Op)}:
			case <-ctx.Done():
				return
			case <-w.done:
				return
			}
			if event.Op&fsnotify.Create != 0 {
				_ = addDirectories(w.impl, event.Name)
			}
		}
	}()
	return out
}
