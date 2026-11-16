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

func TestFSWatcherEvent(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "my-coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler))
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(collDir, "test.yaml"), []byte("data"), 0644))

	select {
	case ev := <-ch:
		assert.Equal(t, "my-coll", ev.Collection)
		assert.Equal(t, FSEventChange, ev.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fs event")
	}
}

func TestFSWatcherNewDirAfterStart(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler))
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	newColl := filepath.Join(dir, "new-coll")
	require.NoError(t, os.MkdirAll(newColl, 0755))

	// The Create event for the top-level dir is FSEventCreate.
	select {
	case ev := <-ch:
		assert.Equal(t, "new-coll", ev.Collection)
		assert.Equal(t, FSEventCreate, ev.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dir create event")
	}

	// Write inside the new dir — the dynamic watch must pick it up as Change.
	require.NoError(t, os.WriteFile(filepath.Join(newColl, "main.js"), []byte("code"), 0644))

	select {
	case ev := <-ch:
		assert.Equal(t, "new-coll", ev.Collection)
		assert.Equal(t, FSEventChange, ev.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for file event inside new dir")
	}
}

func TestFSWatcherDirRemove(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "my-coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler))
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(collDir))

	select {
	case ev := <-ch:
		assert.Equal(t, "my-coll", ev.Collection)
		assert.Equal(t, FSEventRemove, ev.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for remove event")
	}
}

func TestFSWatcherTopLevelRename(t *testing.T) {
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "old-coll")
	require.NoError(t, os.MkdirAll(oldDir, 0755))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler))
	ch, err := w.Start(ctx)
	require.NoError(t, err)

	require.NoError(t, os.Rename(oldDir, filepath.Join(dir, "new-coll")))

	sawRemove := false
	sawCreate := false
	timeout := time.After(2 * time.Second)
	for !sawRemove || !sawCreate {
		select {
		case ev := <-ch:
			if ev.Collection == "old-coll" && ev.Kind == FSEventRemove {
				sawRemove = true
			}
			if ev.Collection == "new-coll" && ev.Kind == FSEventCreate {
				sawCreate = true
			}
		case <-timeout:
			t.Fatalf("timed out: sawRemove=%v sawCreate=%v", sawRemove, sawCreate)
		}
	}
}

func TestFSWatcherNestedRename(t *testing.T) {
	dir := t.TempDir()
	collDir := filepath.Join(dir, "coll")
	require.NoError(t, os.MkdirAll(collDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "old.yaml"), []byte("data"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler))
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
	w := NewFSWatcher(dir, slog.New(slog.DiscardHandler))
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
