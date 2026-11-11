package download

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

func serveBundleHandler(b *bundle.Bundle) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		io.Copy(w, b.Reader())
	})
}

func buildTestBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	pkg := collection.ScriptPackageContent{
		Name:       "test-pkg",
		Entrypoint: "main.js",
		Files: []collection.ScriptFile{
			{Path: "main.js", Content: []byte("function main() {}")},
			{Path: "lib/util.js", Content: []byte("exports.util = {}")},
		},
	}
	b, err := bundle.Build([]collection.ScriptPackageContent{pkg}, "test-pkg", nil, "js", "")
	require.NoError(t, err)
	return b
}

// TestDownload verifies the download, digest verification, and extraction
// pipeline: valid round-trips, digest mismatches, path traversal rejection,
// server errors, and oversized bundles.
func TestDownload(t *testing.T) {
	t.Run("valid bundle", func(t *testing.T) {
		b := buildTestBundle(t)
		srv := httptest.NewServer(serveBundleHandler(b))
		defer srv.Close()

		fs := afero.NewMemMapFs()
		err := Download(context.Background(), srv.Client(), fs, srv.URL, b.Digest, "/bundle")
		require.NoError(t, err)

		data, err := afero.ReadFile(fs, "/bundle/test-pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, "function main() {}", string(data))

		data, err = afero.ReadFile(fs, "/bundle/test-pkg/lib/util.js")
		require.NoError(t, err)
		assert.Equal(t, "exports.util = {}", string(data))

		data, err = afero.ReadFile(fs, "/bundle/manifest.json")
		require.NoError(t, err)
		var manifest bundle.Manifest
		require.NoError(t, json.Unmarshal(data, &manifest))
		assert.Equal(t, "test-pkg/main.js", manifest.Entrypoint)

		data, err = afero.ReadFile(fs, "/bundle/values.json")
		require.NoError(t, err)
		assert.Equal(t, "{}", string(data))
	})

	t.Run("digest mismatch", func(t *testing.T) {
		b := buildTestBundle(t)
		srv := httptest.NewServer(serveBundleHandler(b))
		defer srv.Close()

		fs := afero.NewMemMapFs()
		err := Download(context.Background(), srv.Client(), fs, srv.URL, "bad"+strings.Repeat("0", 61), "/bundle")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "digest mismatch")
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		data := buildTraversalTarGz(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(data)
		}))
		defer srv.Close()

		digest := hashBytes(data)
		fs := afero.NewMemMapFs()
		err := Download(context.Background(), srv.Client(), fs, srv.URL, digest, "/bundle")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsafe tar entry")
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		fs := afero.NewMemMapFs()
		err := Download(context.Background(), srv.Client(), fs, srv.URL, "ignored", "/bundle")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("symlink skipped", func(t *testing.T) {
		data := buildSymlinkTarGz(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(data)
		}))
		defer srv.Close()

		digest := hashBytes(data)
		fs := afero.NewMemMapFs()
		err := Download(context.Background(), srv.Client(), fs, srv.URL, digest, "/bundle")
		require.NoError(t, err)

		exists, err := afero.Exists(fs, "/bundle/link")
		require.NoError(t, err)
		assert.False(t, exists, "symlink entry should be skipped")

		content, err := afero.ReadFile(fs, "/bundle/real.txt")
		require.NoError(t, err)
		assert.Equal(t, "ok", string(content))
	})

	t.Run("bundle too large", func(t *testing.T) {
		if testing.Short() {
			t.Skip("serves >64 MiB of data")
		}
		bigData := make([]byte, collection.MaxBundleSize+1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(bigData)
		}))
		defer srv.Close()

		fs := afero.NewMemMapFs()
		err := Download(context.Background(), srv.Client(), fs, srv.URL, "ignored", "/bundle")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds maximum")
	})
}

func buildSymlinkTarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("ok")
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "real.txt",
		Size: int64(len(content)),
		Mode: 0644,
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     "link",
		Linkname: "/etc/passwd",
	}))

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func buildTraversalTarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("malicious")
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "../../etc/passwd",
		Size: int64(len(content)),
		Mode: 0644,
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}
