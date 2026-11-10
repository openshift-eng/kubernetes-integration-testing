package bundle

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

func testBundle(t *testing.T, name string, content string) *Bundle {
	t.Helper()
	pkg := collection.ScriptPackageContent{
		Name:       name,
		Entrypoint: "main.js",
		Files:      []collection.ScriptFile{{Path: "main.js", Content: []byte(content)}},
	}
	b, err := Build([]collection.ScriptPackageContent{pkg}, name, nil)
	require.NoError(t, err)
	return b
}

// TestBundleStore verifies the store's core operations: put/resolve round-trips,
// content-addressed dedup, self-cleaning of unreferenced digests, disk
// persistence across restarts, and pruning of stale index entries.
func TestBundleStore(t *testing.T) {
	t.Run("put and resolve", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "agent-a", "code-a")
		require.NoError(t, s.Put("agent-a", b))

		digest, ok := s.Resolve("agent-a")
		assert.True(t, ok)
		assert.Equal(t, b.Digest, digest)

		reader, size, err := s.Open(digest)
		require.NoError(t, err)
		defer reader.Close()
		assert.Greater(t, size, int64(0))

		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, b.Size(), len(data))
	})

	t.Run("put idempotent", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "agent-a", "code-a")
		require.NoError(t, s.Put("agent-a", b))
		require.NoError(t, s.Put("agent-a", b))

		digest, ok := s.Resolve("agent-a")
		assert.True(t, ok)
		assert.Equal(t, b.Digest, digest)
	})

	t.Run("two agents same digest", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "shared", "same-code")
		require.NoError(t, s.Put("agent-a", b))
		require.NoError(t, s.Put("agent-b", b))

		entries, err := afero.ReadDir(fs, "/bundles")
		require.NoError(t, err)
		tarCount := 0
		for _, e := range entries {
			if e.Name() != "index.json" {
				tarCount++
			}
		}
		assert.Equal(t, 1, tarCount, "same digest should produce one file")
	})

	t.Run("put replaces and cleans old", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, s.Put("agent-a", old))

		oldPath := "/bundles/" + old.Digest + ".tar.gz"
		exists, err := afero.Exists(fs, oldPath)
		require.NoError(t, err)
		assert.True(t, exists)

		updated := testBundle(t, "pkg", "new-code")
		require.NoError(t, s.Put("agent-a", updated))

		exists, err = afero.Exists(fs, oldPath)
		require.NoError(t, err)
		assert.False(t, exists)

		exists, err = afero.Exists(fs, "/bundles/"+updated.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("remove", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, s.Put("agent-a", b))
		require.NoError(t, s.Remove("agent-a"))

		_, ok := s.Resolve("agent-a")
		assert.False(t, ok)

		exists, err := afero.Exists(fs, "/bundles/"+b.Digest+".tar.gz")
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("remove shared digest keeps file", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "shared", "same-code")
		require.NoError(t, s.Put("agent-a", b))
		require.NoError(t, s.Put("agent-b", b))
		require.NoError(t, s.Remove("agent-a"))

		exists, err := afero.Exists(fs, "/bundles/"+b.Digest+".tar.gz")
		require.NoError(t, err)
		assert.True(t, exists)

		digest, ok := s.Resolve("agent-b")
		assert.True(t, ok)
		assert.Equal(t, b.Digest, digest)
	})

	t.Run("restart persistence", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s1, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, s1.Put("agent-a", b))

		s2, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		digest, ok := s2.Resolve("agent-a")
		assert.True(t, ok)
		assert.Equal(t, b.Digest, digest)
	})

	t.Run("startup prunes stale", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		require.NoError(t, fs.MkdirAll("/bundles", 0755))

		idx := storeIndex{Agents: map[string]string{"stale": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}}
		data, _ := json.Marshal(idx)
		require.NoError(t, afero.WriteFile(fs, "/bundles/index.json", data, 0644))

		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		_, ok := s.Resolve("stale")
		assert.False(t, ok, "entry referencing missing tar.gz should be pruned")
	})

	t.Run("corrupt index starts fresh", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		require.NoError(t, fs.MkdirAll("/bundles", 0755))
		require.NoError(t, afero.WriteFile(fs, "/bundles/index.json", []byte("{invalid"), 0644))

		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, s.Put("agent-a", b))
		digest, ok := s.Resolve("agent-a")
		assert.True(t, ok)
		assert.Equal(t, b.Digest, digest)
	})

	t.Run("invalid digest in index skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		require.NoError(t, fs.MkdirAll("/bundles", 0755))

		idx := storeIndex{Agents: map[string]string{"bad": "../../etc/passwd"}}
		data, _ := json.Marshal(idx)
		require.NoError(t, afero.WriteFile(fs, "/bundles/index.json", data, 0644))

		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		_, ok := s.Resolve("bad")
		assert.False(t, ok, "entry with traversal digest should be skipped")
	})
}
