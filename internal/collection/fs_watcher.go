package collection

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

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
//
// Change and Remove events are debounced per-collection: multiple raw
// events within the debounce window collapse into a single FSEvent
// whose Kind is decided by whether the top-level directory still exists
// at flush time. Create events are sent immediately (so the caller can
// add watches and auto-create resources), followed by a trailing
// debounced Change that covers directories created already populated.
//
// All subdirectories are watched recursively; nested events resolve to
// the owning top-level collection and debounce as Change. Symlinks are
// not followed.
type FSWatcher struct {
	collectionsDir string
	log            *slog.Logger
	debounce       time.Duration
}

// NewFSWatcher creates a watcher for the given collections directory.
func NewFSWatcher(collectionsDir string, log *slog.Logger, debounce time.Duration) *FSWatcher {
	return &FSWatcher{
		collectionsDir: collectionsDir,
		log:            log,
		debounce:       debounce,
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

	watched := map[string]struct{}{w.collectionsDir: {}}
	w.addSubtree(watcher, watched, w.collectionsDir)

	ch := make(chan FSEvent)
	go w.loop(ctx, watcher, ch, watched)
	return ch, nil
}

// addSubtree recursively adds all directories under root (inclusive) to
// the watcher and tracked set, skipping symlinks.
func (w *FSWatcher) addSubtree(watcher *fsnotify.Watcher, watched map[string]struct{}, root string) {
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if err := watcher.Add(path); err != nil {
			w.log.Warn("watching directory", "dir", path, "error", err)
		}
		watched[path] = struct{}{}
		return nil
	})
}

// removeSubtree removes the exact path and all descendants (prefix +
// separator) from the watcher and tracked set.
func removeSubtree(watcher *fsnotify.Watcher, watched map[string]struct{}, root string) {
	prefix := root + string(filepath.Separator)
	for path := range watched {
		if path == root || strings.HasPrefix(path, prefix) {
			watcher.Remove(path)
			delete(watched, path)
		}
	}
}

type pendingFlush struct {
	collection string
	gen        int
}

func (w *FSWatcher) loop(ctx context.Context, watcher *fsnotify.Watcher, ch chan<- FSEvent, watched map[string]struct{}) {
	defer func() {
		if err := watcher.Close(); err != nil {
			w.log.Warn("failed to close watcher", "error", err)
		}
	}()
	defer close(ch)

	timers := make(map[string]*time.Timer)
	gens := make(map[string]int)
	pending := make(chan pendingFlush, 16)

	defer func() {
		for _, t := range timers {
			t.Stop()
		}
	}()

	resetTimer := func(name string) {
		if t, ok := timers[name]; ok {
			t.Stop()
		}
		gens[name]++
		gen := gens[name]
		timers[name] = time.AfterFunc(w.debounce, func() {
			select {
			case pending <- pendingFlush{collection: name, gen: gen}:
			case <-ctx.Done():
			}
		})
	}

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

			// Collections are directories; ignore top-level regular files.
			if w.isTopLevel(ev.Name, name) {
				if ev.Op&fsnotify.Create != 0 {
					if info, err := os.Lstat(ev.Name); err != nil || !info.IsDir() {
						continue
					}
				} else if _, ok := watched[ev.Name]; !ok {
					continue
				}
			}

			// Watch management: register new directory subtrees, clean up deleted ones.
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
					w.addSubtree(watcher, watched, ev.Name)
				}
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				if _, ok := watched[ev.Name]; ok {
					removeSubtree(watcher, watched, ev.Name)
				}
			}

			kind := w.classifyEvent(ev, name)
			if kind == FSEventCreate {
				select {
				case ch <- FSEvent{Collection: name, Kind: FSEventCreate}:
				case <-ctx.Done():
					return
				}
				resetTimer(name)
				continue
			}

			resetTimer(name)

		case flush := <-pending:
			if gens[flush.collection] != flush.gen {
				continue
			}
			delete(timers, flush.collection)
			delete(gens, flush.collection)

			kind := FSEventChange
			if _, err := os.Lstat(filepath.Join(w.collectionsDir, flush.collection)); os.IsNotExist(err) {
				kind = FSEventRemove
			}
			select {
			case ch <- FSEvent{Collection: flush.collection, Kind: kind}:
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
