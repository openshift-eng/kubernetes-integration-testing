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

func TestServer(t *testing.T) {
	t.Run("serve bundle by digest", func(t *testing.T) {
		srv, store := testServer(t)

		pkg := collection.ScriptPackageContent{
			Name:       "test-pkg",
			Entrypoint: "main.js",
			Files:      []collection.ScriptFile{{Path: "main.js", Content: []byte("code")}},
		}
		b, err := Build([]collection.ScriptPackageContent{pkg}, "test-pkg", nil, "js", nil)
		require.NoError(t, err)
		require.NoError(t, store.Put("my-agent", b))

		req := httptest.NewRequest("GET", "/bundles/"+b.Digest, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		body, _ := io.ReadAll(w.Body)
		assert.Equal(t, b.Size(), len(body))
	})

	t.Run("unknown digest", func(t *testing.T) {
		srv, _ := testServer(t)

		req := httptest.NewRequest("GET", "/bundles/"+
			"0000000000000000000000000000000000000000000000000000000000000000", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("invalid digest returns 400", func(t *testing.T) {
		srv, _ := testServer(t)

		req := httptest.NewRequest("GET", "/bundles/not-a-valid-digest", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("content headers", func(t *testing.T) {
		srv, store := testServer(t)

		pkg := collection.ScriptPackageContent{
			Name:       "hdr-pkg",
			Entrypoint: "main.js",
			Files:      []collection.ScriptFile{{Path: "main.js", Content: []byte("x")}},
		}
		b, err := Build([]collection.ScriptPackageContent{pkg}, "hdr-pkg", nil, "js", nil)
		require.NoError(t, err)
		require.NoError(t, store.Put("hdr-agent", b))

		req := httptest.NewRequest("GET", "/bundles/"+b.Digest, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assert.Equal(t, "application/gzip", w.Header().Get("Content-Type"))
		assert.NotEmpty(t, w.Header().Get("Content-Length"))
		assert.Equal(t, b.Digest, w.Header().Get("ETag"))
		assert.Equal(t, "public, max-age=31536000, immutable", w.Header().Get("Cache-Control"))
	})

	t.Run("old and new digest both servable after put", func(t *testing.T) {
		srv, store := testServer(t)

		old := testBundle(t, "pkg", "old-code")
		require.NoError(t, store.Put("agent-a", old))

		updated := testBundle(t, "pkg", "new-code")
		require.NoError(t, store.Put("agent-a", updated))

		for _, b := range []*Bundle{old, updated} {
			req := httptest.NewRequest("GET", "/bundles/"+b.Digest, nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code, "digest %s should be servable", b.Digest[:12])
		}
	})
}
