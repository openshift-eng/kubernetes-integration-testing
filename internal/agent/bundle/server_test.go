package bundle

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

func testServer(t *testing.T) (*Server, *BundleStore) {
	t.Helper()
	fs := afero.NewMemMapFs()
	store, err := NewBundleStore(fs, "/bundles", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	srv := NewServer(store, ":0", slog.New(slog.DiscardHandler))
	return srv, store
}

// TestServer verifies the bundle HTTP server: serving bundles, returning 404
// for unknown agents, and setting correct response headers.
func TestServer(t *testing.T) {
	t.Run("serve bundle", func(t *testing.T) {
		srv, store := testServer(t)

		pkg := collection.ScriptPackageContent{
			Name:       "test-pkg",
			Entrypoint: "main.js",
			Files:      []collection.ScriptFile{{Path: "main.js", Content: []byte("code")}},
		}
		b, err := Build([]collection.ScriptPackageContent{pkg}, "test-pkg", nil)
		require.NoError(t, err)
		require.NoError(t, store.Put("my-agent", b))

		req := httptest.NewRequest("GET", "/agents/my-agent/bundle", nil)
		w := httptest.NewRecorder()
		srv.server.Handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		body, _ := io.ReadAll(w.Body)
		assert.Equal(t, b.Size(), len(body))
	})

	t.Run("unknown agent", func(t *testing.T) {
		srv, _ := testServer(t)

		req := httptest.NewRequest("GET", "/agents/nonexistent/bundle", nil)
		w := httptest.NewRecorder()
		srv.server.Handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("content headers", func(t *testing.T) {
		srv, store := testServer(t)

		pkg := collection.ScriptPackageContent{
			Name:       "hdr-pkg",
			Entrypoint: "main.js",
			Files:      []collection.ScriptFile{{Path: "main.js", Content: []byte("x")}},
		}
		b, err := Build([]collection.ScriptPackageContent{pkg}, "hdr-pkg", nil)
		require.NoError(t, err)
		require.NoError(t, store.Put("hdr-agent", b))

		req := httptest.NewRequest("GET", "/agents/hdr-agent/bundle", nil)
		w := httptest.NewRecorder()
		srv.server.Handler.ServeHTTP(w, req)

		assert.Equal(t, "application/gzip", w.Header().Get("Content-Type"))
		assert.NotEmpty(t, w.Header().Get("Content-Length"))
		assert.Equal(t, b.Digest, w.Header().Get("ETag"))
	})
}
