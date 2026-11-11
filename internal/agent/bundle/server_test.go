package bundle

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

	t.Run("WaitReady succeeds after Start", func(t *testing.T) {
		srv, _ := testServer(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.Start(ctx)
		}()

		require.NoError(t, srv.WaitReady(ctx))
		require.NoError(t, srv.Ready(nil))

		cancel()
		<-done

		assert.Error(t, srv.Ready(nil), "ready must be false after Stop")
		assert.Error(t, srv.WaitReady(context.Background()), "WaitReady must fail after Stop")
	})

	t.Run("WaitReady fails on listener error", func(t *testing.T) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer lis.Close()

		blocked := NewServer(nil, lis.Addr().String(), slog.New(slog.DiscardHandler))

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- blocked.Start(ctx)
		}()

		err = blocked.WaitReady(ctx)
		assert.Error(t, err)
		assert.Error(t, blocked.Ready(nil))

		startErr := <-done
		assert.Error(t, startErr)
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
