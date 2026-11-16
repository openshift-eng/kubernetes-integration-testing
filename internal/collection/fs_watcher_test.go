package collection

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDebounce = 20 * time.Millisecond

func nextEvent(t *testing.T, ch <-chan FSEvent) FSEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return FSEvent{}
	}
}

func TestFSWatcherEvent(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "my-coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(collDir, "test.yaml"), []byte("data"), 0644))

	ev := nextEvent(t, ch)
	assert.Equal(t, "my-coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherNewDirAfterStart(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	newColl := filepath.Join(dir, "new-coll")
	require.NoError(t, os.MkdirAll(newColl, 0755))

	// Create is immediate.
	ev := nextEvent(t, ch)
	require.Equal(t, "new-coll", ev.Collection)
	require.Equal(t, FSEventCreate, ev.Kind)

	// Drain trailing Change so the next write proves dynamic watch.
	ev = nextEvent(t, ch)
	require.Equal(t, FSEventChange, ev.Kind)

	// Write inside the new dir — only arrives if watcher.Add succeeded.
	require.NoError(t, os.WriteFile(filepath.Join(newColl, "main.js"), []byte("code"), 0644))

	ev = nextEvent(t, ch)
	assert.Equal(t, "new-coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherDirRemove(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "my-coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(collDir))

	ev := nextEvent(t, ch)
	assert.Equal(t, "my-coll", ev.Collection)
	assert.Equal(t, FSEventRemove, ev.Kind)
}

// Top-level rename produces Remove(old) + Create(new) + trailing Change(new).
// Subdirectory watches are cleaned up for the old name and re-registered for
// the new one, so writes in the renamed subtree resolve to the new collection.
func TestFSWatcherTopLevelRenameWithSubdirs(t *testing.T) {
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "old-coll")
	sub := filepath.Join(oldDir, "scripts", "pkg")
	require.NoError(t, os.MkdirAll(sub, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "main.js"), []byte("code"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.Rename(oldDir, filepath.Join(dir, "new-coll")))

	// Consume Remove(old) + Create(new) + trailing Change(new).
	sawRemove := false
	sawCreate := false
	sawTrailing := false
	timeout := time.After(2 * time.Second)
	for !sawRemove || !sawCreate || !sawTrailing {
		select {
		case ev := <-ch:
			if ev.Collection == "old-coll" && ev.Kind == FSEventRemove {
				sawRemove = true
			}
			if ev.Collection == "new-coll" && ev.Kind == FSEventCreate {
				sawCreate = true
			}
			if ev.Collection == "new-coll" && ev.Kind == FSEventChange {
				sawTrailing = true
			}
		case <-timeout:
			t.Fatalf("timed out: sawRemove=%v sawCreate=%v sawTrailing=%v", sawRemove, sawCreate, sawTrailing)
		}
	}

	// Write inside the renamed subtree — must resolve to new-coll, not old-coll.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new-coll", "scripts", "pkg", "main.js"), []byte("updated"), 0644))

	ev := nextEvent(t, ch)
	assert.Equal(t, "new-coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherNestedRename(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "old.yaml"), []byte("data"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.Rename(filepath.Join(collDir, "old.yaml"), filepath.Join(collDir, "new.yaml")))

	sawChange := false
	timeout := time.After(2 * time.Second)
	for !sawChange {
		select {
		case ev := <-ch:
			if ev.Collection == "coll" && ev.Kind == FSEventChange {
				sawChange = true
			}
		case <-timeout:
			t.Fatal("timed out waiting for Change event from nested rename")
		}
	}
}

func TestFSWatcherCleanShutdown(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "coll"), 0755))

	ctx, cancel := context.WithCancel(context.Background())
	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	cancel()

	drained := false
	timeout := time.After(2 * time.Second)
	for !drained {
		select {
		case _, ok := <-ch:
			if !ok {
				drained = true
			}
		case <-timeout:
			t.Fatal("channel not closed after context cancellation")
		}
	}
}

func TestFSWatcherTopLevelFileIgnored(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	// Top-level regular file — must be silently ignored.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray-file"), []byte("data"), 0644))

	// Write inside a real collection to prove the watcher is alive.
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "f.yaml"), []byte("data"), 0644))

	ev := nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherDebounceCollapsesSameCollection(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "a.yaml"), []byte("v1"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	for i := range 5 {
		require.NoError(t, os.WriteFile(filepath.Join(collDir, "a.yaml"), []byte("v"+string(rune('1'+i))), 0644))
		time.Sleep(2 * time.Millisecond)
	}

	ev := nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)

	select {
	case ev := <-ch:
		t.Fatalf("expected no more events, got %+v", ev)
	case <-time.After(testDebounce * 3):
	}
}

func TestFSWatcherDebounceIndependentCollections(t *testing.T) {
	dir := t.TempDir()
	coll1 := filepath.Join(dir, "coll-a")
	coll2 := filepath.Join(dir, "coll-b")
	require.NoError(t, os.MkdirAll(coll1, 0755))
	require.NoError(t, os.MkdirAll(coll2, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(coll1, "f.yaml"), []byte("a"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(coll2, "f.yaml"), []byte("b"), 0644))

	saw := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for len(saw) < 2 {
		select {
		case ev := <-ch:
			if ev.Kind == FSEventChange {
				saw[ev.Collection] = true
			}
		case <-timeout:
			t.Fatalf("timed out: got %v", saw)
		}
	}
	assert.True(t, saw["coll-a"])
	assert.True(t, saw["coll-b"])
}

func TestFSWatcherCreateImmediate(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), 500*time.Millisecond)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "new-coll"), 0755))

	start := time.Now()
	ev := nextEvent(t, ch)
	assert.Equal(t, "new-coll", ev.Collection)
	assert.Equal(t, FSEventCreate, ev.Kind)
	assert.Less(t, time.Since(start), 200*time.Millisecond)
}

// Rename a pre-populated directory into the watched root. The Create is
// immediate and the trailing Change covers the existing content without
// needing individual file-write events.
func TestFSWatcherCreatePopulatedDirEndsInChange(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	staging := filepath.Join(t.TempDir(), "new-coll")
	require.NoError(t, os.MkdirAll(staging, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "file.yaml"), []byte("data"), 0644))
	require.NoError(t, os.Rename(staging, filepath.Join(dir, "new-coll")))

	ev := nextEvent(t, ch)
	require.Equal(t, "new-coll", ev.Collection)
	require.Equal(t, FSEventCreate, ev.Kind)

	ev = nextEvent(t, ch)
	assert.Equal(t, "new-coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherChangeThenRemoveEndsInRemove(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "f.yaml"), []byte("v1"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(collDir, "f.yaml"), []byte("v2"), 0644))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, os.RemoveAll(collDir))

	ev := nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventRemove, ev.Kind)
}

func TestFSWatcherRemoveThenRecreateEndsInChange(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(collDir))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, os.MkdirAll(collDir, 0755))

	// Create is immediate for the recreate.
	ev := nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventCreate, ev.Kind)

	// The debounced flush resolves to Change because dir exists.
	ev = nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherNestedFileChange(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "coll", "sub", "deep")
	require.NoError(t, os.MkdirAll(nested, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "f.yaml"), []byte("v1"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(nested, "f.yaml"), []byte("v2"), 0644))

	ev := nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherNestedDirCreateAndWrite(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	// Create a nested subdir, then write inside it.
	sub := filepath.Join(collDir, "scripts")
	require.NoError(t, os.MkdirAll(sub, 0755))

	// Drain the Change from the subdir creation.
	ev := nextEvent(t, ch)
	require.Equal(t, "coll", ev.Collection)
	require.Equal(t, FSEventChange, ev.Kind)

	// Write inside the new subdir — proves addSubtree registered it.
	require.NoError(t, os.WriteFile(filepath.Join(sub, "main.js"), []byte("code"), 0644))

	ev = nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherNestedDirRemove(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "coll", "scripts")
	require.NoError(t, os.MkdirAll(sub, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "main.js"), []byte("code"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(sub))

	ev := nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}

func TestFSWatcherSymlinkIgnored(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))

	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, "secret.yaml"), []byte("data"), 0644))

	require.NoError(t, os.Symlink(target, filepath.Join(collDir, "link")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler), testDebounce)
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	// Write to symlink target — must not produce an event.
	require.NoError(t, os.WriteFile(filepath.Join(target, "secret.yaml"), []byte("changed"), 0644))

	// Write inside the real collection to prove the watcher is alive.
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "real.yaml"), []byte("data"), 0644))

	ev := nextEvent(t, ch)
	assert.Equal(t, "coll", ev.Collection)
	assert.Equal(t, FSEventChange, ev.Kind)
}
