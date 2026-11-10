package bundle

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeState(agents []string, protected []string) ProtectedState {
	return func(_ context.Context) (ClusterSnapshot, error) {
		snap := ClusterSnapshot{
			ActiveAgents:     make(map[string]struct{}, len(agents)),
			ProtectedDigests: make(map[string]struct{}, len(protected)),
		}
		for _, a := range agents {
			snap.ActiveAgents[a] = struct{}{}
		}
		for _, d := range protected {
			snap.ProtectedDigests[d] = struct{}{}
		}
		return snap, nil
	}
}

func TestGC(t *testing.T) {
	ctx := context.Background()
	retention := 10 * time.Minute

	t.Run("skips first tick", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, store.Put("agent-a", old))

		fresh := testBundle(t, "pkg", "new-code")
		require.NoError(t, store.Put("agent-a", fresh))

		gc := NewGCRunner(store, makeState([]string{"agent-a"}, nil))
		gc.Tick(ctx, time.Now(), retention)

		exists, err := afero.Exists(fs, "/bundles/"+old.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists, "first tick must never delete")
	})

	t.Run("respects retention", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, store.Put("agent-a", old))

		fresh := testBundle(t, "pkg", "new-code")
		require.NoError(t, store.Put("agent-a", fresh))

		gc := NewGCRunner(store, makeState([]string{"agent-a"}, nil))
		now := time.Now()

		gc.Tick(ctx, now, retention)

		gc.Tick(ctx, now.Add(retention/2), retention)
		exists, err := afero.Exists(fs, "/bundles/"+old.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists, "orphan must survive within retention window")

		gc.Tick(ctx, now.Add(retention+time.Second), retention)
		exists, err = afero.Exists(fs, "/bundles/"+old.Digest+".tar.gz")
		require.NoError(t, err)
		assert.False(t, exists, "orphan must be removed after retention expires")

		freshExists, err := afero.Exists(fs, "/bundles/"+fresh.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, freshExists, "active blob must survive")
	})

	t.Run("active digest in mapping never collected", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, store.Put("agent-a", b))

		gc := NewGCRunner(store, makeState([]string{"agent-a"}, nil))
		now := time.Now()

		gc.Tick(ctx, now, retention)
		gc.Tick(ctx, now.Add(retention*2), retention)

		exists, err := afero.Exists(fs, "/bundles/"+b.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists, "blob referenced by agent mapping must not be collected")
	})

	t.Run("externally protected digest never collected", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, store.Put("agent-a", old))

		fresh := testBundle(t, "pkg", "new-code")
		require.NoError(t, store.Put("agent-a", fresh))

		gc := NewGCRunner(store, makeState([]string{"agent-a"}, []string{old.Digest}))
		now := time.Now()

		gc.Tick(ctx, now, retention)
		gc.Tick(ctx, now.Add(retention*2), retention)

		exists, err := afero.Exists(fs, "/bundles/"+old.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists, "externally protected digest must not be collected")
	})

	t.Run("state callback error skips tick", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, store.Put("agent-a", old))

		fresh := testBundle(t, "pkg", "new-code")
		require.NoError(t, store.Put("agent-a", fresh))

		failing := func(_ context.Context) (ClusterSnapshot, error) {
			return ClusterSnapshot{}, fmt.Errorf("api unavailable")
		}
		gc := NewGCRunner(store, failing)
		now := time.Now()

		gc.Tick(ctx, now, retention)
		gc.Tick(ctx, now.Add(retention*2), retention)

		exists, err := afero.Exists(fs, "/bundles/"+old.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists, "orphan must survive when state callback fails")
	})

	t.Run("collects only stale blobs", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, store.Put("agent-a", old))

		updated := testBundle(t, "pkg", "new-code")
		require.NoError(t, store.Put("agent-a", updated))

		gc := NewGCRunner(store, makeState([]string{"agent-a"}, nil))
		now := time.Now()

		gc.Tick(ctx, now, retention)
		gc.Tick(ctx, now.Add(retention+time.Second), retention)

		oldExists, err := afero.Exists(fs, "/bundles/"+old.Digest+".tar.gz")
		require.NoError(t, err)
		assert.False(t, oldExists, "orphaned old blob must be collected")

		newExists, err := afero.Exists(fs, "/bundles/"+updated.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, newExists, "active blob must survive")
	})

	t.Run("put between ticks protects digest", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, store.Put("agent-a", old))

		fresh := testBundle(t, "pkg", "new-code")
		require.NoError(t, store.Put("agent-a", fresh))

		gc := NewGCRunner(store, makeState([]string{"agent-a"}, nil))
		now := time.Now()
		gc.Tick(ctx, now, retention)

		require.NoError(t, store.Put("agent-a", old))

		gc.Tick(ctx, now.Add(retention+time.Second), retention)
		exists, err := afero.Exists(fs, "/bundles/"+old.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists, "Put between ticks must protect the digest")
	})

	t.Run("purges stale agent mappings then collects blob", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, store.Put("agent-a", b))

		gc := NewGCRunner(store, makeState(nil, nil))
		now := time.Now()

		gc.Tick(ctx, now, retention)

		gc.Tick(ctx, now.Add(retention+time.Second), retention)
		exists, err := afero.Exists(fs, "/bundles/"+b.Digest+".tar.gz")
		require.NoError(t, err)
		assert.False(t, exists, "blob must be collected after mapping purge + retention")
	})

	t.Run("purge after delete prevents recreate race", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, store.Put("agent-a", b))

		// The state callback simulates the race: after snapshotForGC has
		// already run (and didn't see the mapping), the Put re-adds it.
		// deleteIfUnreferenced must revalidate under lock and protect the blob.
		latePut := false
		state := func(_ context.Context) (ClusterSnapshot, error) {
			if latePut {
				require.NoError(t, store.Put("agent-a", b))
			}
			return ClusterSnapshot{
				ActiveAgents:     map[string]struct{}{},
				ProtectedDigests: map[string]struct{}{},
			}, nil
		}

		gc := NewGCRunner(store, state)
		now := time.Now()
		gc.Tick(ctx, now, retention)

		latePut = true
		gc.Tick(ctx, now.Add(retention+time.Second), retention)

		exists, err := afero.Exists(fs, "/bundles/"+b.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists, "blob must survive: Put arrived after snapshot, deleteIfUnreferenced revalidates under lock")
	})

	t.Run("purge preserves live agent mappings", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, store.Put("agent-a", b))
		require.NoError(t, store.Put("agent-b", b))

		gc := NewGCRunner(store, makeState([]string{"agent-a"}, nil))
		now := time.Now()

		gc.Tick(ctx, now, retention)
		gc.Tick(ctx, now.Add(retention+time.Second), retention)

		exists, err := afero.Exists(fs, "/bundles/"+b.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists, "blob must survive while any live agent references it")
	})
}
