package bundle

import (
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
	b, err := Build([]collection.ScriptPackageContent{pkg}, name, nil, "js", nil)
	require.NoError(t, err)
	return b
}

func TestBundleStore(t *testing.T) {
	t.Run("put and open", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "agent-a", "code-a")
		require.NoError(t, s.Put("agent-a", b))

		reader, size, err := s.Open(b.Digest)
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

		reader, _, err := s.Open(b.Digest)
		require.NoError(t, err)
		reader.Close()
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

	t.Run("put keeps old blob", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, s.Put("agent-a", old))

		updated := testBundle(t, "pkg", "new-code")
		require.NoError(t, s.Put("agent-a", updated))

		oldReader, _, err := s.Open(old.Digest)
		require.NoError(t, err, "old blob must survive Put — GC handles cleanup")
		oldReader.Close()

		newReader, _, err := s.Open(updated.Digest)
		require.NoError(t, err)
		newReader.Close()
	})

	t.Run("restart persistence", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		s1, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, s1.Put("agent-a", b))

		s2, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		reader, _, err := s2.Open(b.Digest)
		require.NoError(t, err, "blob must survive restart")
		reader.Close()
	})

	t.Run("startup prunes stale index entry", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		require.NoError(t, fs.MkdirAll("/bundles", 0755))

		require.NoError(t, afero.WriteFile(fs, "/bundles/index.json",
			[]byte(`{"agents":{"stale":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}}`), 0644))

		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, s.Put("stale", b))

		s2, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		reader, _, err := s2.Open(b.Digest)
		require.NoError(t, err, "new mapping for previously stale agent must persist")
		reader.Close()
	})

	t.Run("corrupt index starts fresh", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		require.NoError(t, fs.MkdirAll("/bundles", 0755))
		require.NoError(t, afero.WriteFile(fs, "/bundles/index.json", []byte("{invalid"), 0644))

		s, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		b := testBundle(t, "pkg", "code")
		require.NoError(t, s.Put("agent-a", b))

		reader, _, err := s.Open(b.Digest)
		require.NoError(t, err)
		reader.Close()
	})

	t.Run("invalid digest in index skipped", func(t *testing.T) {
		fs := afero.NewMemMapFs()
		require.NoError(t, fs.MkdirAll("/bundles", 0755))

		require.NoError(t, afero.WriteFile(fs, "/bundles/index.json",
			[]byte(`{"agents":{"bad":"../../etc/passwd"}}`), 0644))

		_, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
		require.NoError(t, err, "store must load despite traversal digest in index")
	})
}
