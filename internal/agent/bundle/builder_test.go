package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

func runnablePackage(name string, files map[string]string, entrypoint string) collection.ScriptPackageContent {
	var sf []collection.ScriptFile
	for path, content := range files {
		sf = append(sf, collection.ScriptFile{Path: path, Content: []byte(content)})
	}
	return collection.ScriptPackageContent{
		Name:       name,
		Entrypoint: entrypoint,
		Files:      sf,
	}
}

func libraryPackage(name string, files map[string]string) collection.ScriptPackageContent {
	var sf []collection.ScriptFile
	for path, content := range files {
		sf = append(sf, collection.ScriptFile{Path: path, Content: []byte(content)})
	}
	return collection.ScriptPackageContent{
		Name:  name,
		Files: sf,
	}
}

func extractTarGz(t *testing.T, b *Bundle) map[string][]byte {
	t.Helper()
	gr, err := gzip.NewReader(b.Reader())
	require.NoError(t, err)
	defer gr.Close()
	tr := tar.NewReader(gr)

	files := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		files[hdr.Name] = data

		assert.Equal(t, int64(0), hdr.ModTime.Unix(), "tar entry %s should have epoch mtime", hdr.Name)
		assert.Equal(t, int64(0644), hdr.Mode, "tar entry %s should have 0644 mode", hdr.Name)
	}
	return files
}

// TestBuild verifies that bundles are built correctly: manifest structure,
// file layout, digest computation, determinism across builds, and values
// serialization.
func TestBuild(t *testing.T) {
	t.Run("single package", func(t *testing.T) {
		pkg := runnablePackage("mcd-stub", map[string]string{
			"main.js":      "function main() {}",
			"lib/state.js": "exports.state = {}",
		}, "main.js")

		b, err := Build([]collection.ScriptPackageContent{pkg}, "mcd-stub", nil, "js", "longRunning", "")
		require.NoError(t, err)
		assert.NotEmpty(t, b.Digest)
		assert.Equal(t, "js", b.Manifest.Runtime)
		assert.Equal(t, "longRunning", b.Manifest.Lifecycle)
		assert.Empty(t, b.Manifest.Timeout)
		assert.Equal(t, "mcd-stub/main.js", b.Manifest.Entrypoint)
		require.Len(t, b.Manifest.Packages, 1)
		assert.Equal(t, "mcd-stub", b.Manifest.Packages[0].Name)

		files := extractTarGz(t, b)
		assert.Contains(t, files, "manifest.json")
		assert.Contains(t, files, "values.json")
		assert.Contains(t, files, "mcd-stub/main.js")
		assert.Contains(t, files, "mcd-stub/lib/state.js")
	})

	t.Run("multi package", func(t *testing.T) {
		runnable := runnablePackage("mcd-stub", map[string]string{
			"main.js": "main",
		}, "main.js")
		lib := libraryPackage("common", map[string]string{
			"helpers.js": "exports.help = {}",
		})

		b, err := Build([]collection.ScriptPackageContent{runnable, lib}, "mcd-stub", nil, "js", "bounded", "30s")
		require.NoError(t, err)
		assert.Equal(t, "bounded", b.Manifest.Lifecycle)
		assert.Equal(t, "30s", b.Manifest.Timeout)
		require.Len(t, b.Manifest.Packages, 2)
		assert.Equal(t, "common", b.Manifest.Packages[0].Name)
		assert.Equal(t, "mcd-stub", b.Manifest.Packages[1].Name)
		assert.Empty(t, b.Manifest.Packages[0].Entrypoint)

		files := extractTarGz(t, b)
		assert.Contains(t, files, "common/helpers.js")
		assert.Contains(t, files, "mcd-stub/main.js")
	})

	t.Run("deterministic digest", func(t *testing.T) {
		pkg := runnablePackage("pkg", map[string]string{"main.js": "code"}, "main.js")

		b1, err := Build([]collection.ScriptPackageContent{pkg}, "pkg", nil, "js", "longRunning", "")
		require.NoError(t, err)
		b2, err := Build([]collection.ScriptPackageContent{pkg}, "pkg", nil, "js", "longRunning", "")
		require.NoError(t, err)

		assert.Equal(t, b1.Digest, b2.Digest)
	})

	t.Run("reordered packages", func(t *testing.T) {
		a := runnablePackage("aaa", map[string]string{"main.js": "a"}, "main.js")
		b := libraryPackage("bbb", map[string]string{"lib.js": "b"})

		b1, err := Build([]collection.ScriptPackageContent{a, b}, "aaa", nil, "js", "longRunning", "")
		require.NoError(t, err)
		b2, err := Build([]collection.ScriptPackageContent{b, a}, "aaa", nil, "js", "longRunning", "")
		require.NoError(t, err)

		assert.Equal(t, b1.Digest, b2.Digest)
	})

	t.Run("values serialized", func(t *testing.T) {
		pkg := runnablePackage("pkg", map[string]string{"main.js": "code"}, "main.js")
		values := map[string]json.RawMessage{
			"replicas": json.RawMessage(`3`),
			"name":     json.RawMessage(`"test"`),
		}

		b, err := Build([]collection.ScriptPackageContent{pkg}, "pkg", values, "js", "longRunning", "")
		require.NoError(t, err)

		files := extractTarGz(t, b)
		var parsed map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(files["values.json"], &parsed))
		assert.JSONEq(t, `3`, string(parsed["replicas"]))
		assert.JSONEq(t, `"test"`, string(parsed["name"]))
	})

	t.Run("empty values", func(t *testing.T) {
		pkg := runnablePackage("pkg", map[string]string{"main.js": "code"}, "main.js")

		b, err := Build([]collection.ScriptPackageContent{pkg}, "pkg", nil, "js", "longRunning", "")
		require.NoError(t, err)

		files := extractTarGz(t, b)
		assert.Equal(t, "{}", string(files["values.json"]))
	})
}

// TestBuildValidation verifies that invalid inputs are rejected: missing
// entrypoint package, library-only entrypoint, too many packages, oversized
// bundles, and empty package lists.
func TestBuildValidation(t *testing.T) {
	t.Run("missing entrypointPackage", func(t *testing.T) {
		pkg := runnablePackage("pkg", map[string]string{"main.js": "code"}, "main.js")

		_, err := Build([]collection.ScriptPackageContent{pkg}, "nonexistent", nil, "js", "longRunning", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "entrypoint package")
	})

	t.Run("entrypointPackage no entrypoint file", func(t *testing.T) {
		lib := libraryPackage("pkg", map[string]string{"util.js": "exports = {}"})

		_, err := Build([]collection.ScriptPackageContent{lib}, "pkg", nil, "js", "longRunning", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no entrypoint")
	})

	t.Run("too many packages", func(t *testing.T) {
		var packages []collection.ScriptPackageContent
		for i := 0; i <= collection.MaxBundlePackages; i++ {
			packages = append(packages, runnablePackage(
				strings.Repeat("p", i+1),
				map[string]string{"main.js": "x"},
				"main.js",
			))
		}

		_, err := Build(packages, "p", nil, "js", "longRunning", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too many packages")
	})

	t.Run("exceeds MaxBundleSize", func(t *testing.T) {
		if testing.Short() {
			t.Skip("allocates >64 MiB of random data")
		}
		rng := rand.New(rand.NewSource(0))
		data := make([]byte, collection.MaxBundleSize+1)
		rng.Read(data)
		pkg := runnablePackage("pkg", map[string]string{"main.js": string(data)}, "main.js")

		_, err := Build([]collection.ScriptPackageContent{pkg}, "pkg", nil, "js", "longRunning", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds maximum")
	})

	t.Run("empty packages", func(t *testing.T) {
		_, err := Build(nil, "pkg", nil, "js", "longRunning", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no packages")
	})
}
