package collection

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// FSEventKind describes the type of filesystem change.
type FSEventKind int

const (
	FSEventCreate FSEventKind = iota
	FSEventChange
	FSEventRemove
)

// FSEvent represents a filesystem change resolved to a collection name.
type FSEvent struct {
	Collection string
	Kind       FSEventKind
}

// FSWatcher watches a collections directory for filesystem changes and
// emits FSEvents on a channel. It owns the fsnotify lifecycle and the
// event loop goroutine. Controller-specific logic (e.g. GenericEvent
// conversion) belongs in the caller.
type FSWatcher struct {
	collectionsDir string
	log            *slog.Logger
}

// NewFSWatcher creates a watcher for the given collections directory.
func NewFSWatcher(collectionsDir string, log *slog.Logger) *FSWatcher {
	return &FSWatcher{
		collectionsDir: collectionsDir,
		log:            log,
	}
}

// Start begins watching and returns the event channel. The goroutine
// stops when ctx is cancelled; the channel is closed on exit.
func (w *FSWatcher) Start(ctx context.Context) (<-chan FSEvent, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := watcher.Add(w.collectionsDir); err != nil {
		if closeErr := watcher.Close(); closeErr != nil {
			w.log.Warn("failed to close watcher", "error", closeErr)
		}
		return nil, err
	}

	entries, err := os.ReadDir(w.collectionsDir)
	if err != nil {
		w.log.Warn("reading collections directory for initial watch", "dir", w.collectionsDir, "error", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			dir := filepath.Join(w.collectionsDir, e.Name())
			if err := watcher.Add(dir); err != nil {
				w.log.Warn("watching collection directory", "dir", dir, "error", err)
			}
		}
	}

	ch := make(chan FSEvent)
	go w.loop(ctx, watcher, ch)
	return ch, nil
}

func (w *FSWatcher) loop(ctx context.Context, watcher *fsnotify.Watcher, ch chan<- FSEvent) {
	defer func() {
		if err := watcher.Close(); err != nil {
			w.log.Warn("failed to close watcher", "error", err)
		}
	}()
	defer close(ch)

	for {
		select {
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			name := w.resolveCollectionName(ev.Name)
			if name == "" {
				continue
			}

			kind := w.classifyEvent(ev, name)
			if kind == FSEventCreate {
				if err := watcher.Add(ev.Name); err != nil {
					w.log.Warn("watching new collection directory", "dir", ev.Name, "error", err)
				}
			}

			select {
			case ch <- FSEvent{Collection: name, Kind: kind}:
			case <-ctx.Done():
				return
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			w.log.Warn("fsnotify error", "error", err)

		case <-ctx.Done():
			return
		}
	}
}

// classifyEvent maps an fsnotify event to the semantic FSEventKind.
// Top-level creates → FSEventCreate, top-level removes/renames →
// FSEventRemove, everything else → FSEventChange.
func (w *FSWatcher) classifyEvent(ev fsnotify.Event, collectionName string) FSEventKind {
	if !w.isTopLevel(ev.Name, collectionName) {
		return FSEventChange
	}
	if ev.Op&fsnotify.Create != 0 {
		return FSEventCreate
	}
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		return FSEventRemove
	}
	return FSEventChange
}

func (w *FSWatcher) isTopLevel(path, collectionName string) bool {
	rel, _ := filepath.Rel(w.collectionsDir, path)
	return rel == collectionName
}

// resolveCollectionName extracts the top-level collection directory name
// from an absolute path under collectionsDir.
func (w *FSWatcher) resolveCollectionName(path string) string {
	rel, err := filepath.Rel(w.collectionsDir, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return strings.SplitN(rel, string(filepath.Separator), 2)[0]
}
