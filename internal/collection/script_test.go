package collection

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// TestLoadScriptPackageFromDir verifies that directory-based packages are loaded
// correctly: file discovery, include glob filtering (single, recursive, multi),
// subdirectory preservation, and deterministic sort order.
func TestLoadScriptPackageFromDir(t *testing.T) {
	t.Run("all files", func(t *testing.T) {
		fs := fstest.MapFS{
			"scripts/mcd-stub/main.js":      &fstest.MapFile{Data: []byte(`function main(ctx) {}`)},
			"scripts/mcd-stub/lib/state.js": &fstest.MapFile{Data: []byte(`exports.state = {}`)},
			"scripts/mcd-stub/lib/kube.js":  &fstest.MapFile{Data: []byte(`exports.kube = {}`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "mcd-stub",
			Root:       "scripts/mcd-stub",
			Entrypoint: "main.js",
		}

		content, err := LoadScriptPackage(pkg, fs)
		require.NoError(t, err)
		assert.Equal(t, "mcd-stub", content.Name)
		assert.Equal(t, "main.js", content.Entrypoint)
		require.Len(t, content.Files, 3)
		assert.Equal(t, "lib/kube.js", content.Files[0].Path)
		assert.Equal(t, "lib/state.js", content.Files[1].Path)
		assert.Equal(t, "main.js", content.Files[2].Path)
	})

	t.Run("single glob", func(t *testing.T) {
		fs := fstest.MapFS{
			"scripts/pkg/main.js":     &fstest.MapFile{Data: []byte(`main`)},
			"scripts/pkg/helper.js":   &fstest.MapFile{Data: []byte(`helper`)},
			"scripts/pkg/README.md":   &fstest.MapFile{Data: []byte(`docs`)},
			"scripts/pkg/data.json":   &fstest.MapFile{Data: []byte(`{}`)},
			"scripts/pkg/lib/util.js": &fstest.MapFile{Data: []byte(`util`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "pkg",
			Root:       "scripts/pkg",
			Include:    []string{"*.js"},
			Entrypoint: "main.js",
		}

		content, err := LoadScriptPackage(pkg, fs)
		require.NoError(t, err)
		require.Len(t, content.Files, 2)
		assert.Equal(t, "helper.js", content.Files[0].Path)
		assert.Equal(t, "main.js", content.Files[1].Path)
	})

	t.Run("recursive glob", func(t *testing.T) {
		fs := fstest.MapFS{
			"scripts/pkg/main.js":       &fstest.MapFile{Data: []byte(`main`)},
			"scripts/pkg/helper.js":     &fstest.MapFile{Data: []byte(`helper`)},
			"scripts/pkg/README.md":     &fstest.MapFile{Data: []byte(`docs`)},
			"scripts/pkg/lib/state.js":  &fstest.MapFile{Data: []byte(`state`)},
			"scripts/pkg/lib/deep/a.js": &fstest.MapFile{Data: []byte(`deep`)},
			"scripts/pkg/data.json":     &fstest.MapFile{Data: []byte(`{}`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "pkg",
			Root:       "scripts/pkg",
			Include:    []string{"**/*.js"},
			Entrypoint: "main.js",
		}

		content, err := LoadScriptPackage(pkg, fs)
		require.NoError(t, err)
		require.Len(t, content.Files, 4)
		assert.Equal(t, "helper.js", content.Files[0].Path)
		assert.Equal(t, "lib/deep/a.js", content.Files[1].Path)
		assert.Equal(t, "lib/state.js", content.Files[2].Path)
		assert.Equal(t, "main.js", content.Files[3].Path)
	})

	t.Run("multiple globs", func(t *testing.T) {
		fs := fstest.MapFS{
			"pkg/main.js":     &fstest.MapFile{Data: []byte(`main`)},
			"pkg/lib/util.js": &fstest.MapFile{Data: []byte(`util`)},
			"pkg/README.md":   &fstest.MapFile{Data: []byte(`docs`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "pkg",
			Root:       "pkg",
			Include:    []string{"*.js", "lib/*.js"},
			Entrypoint: "main.js",
		}

		content, err := LoadScriptPackage(pkg, fs)
		require.NoError(t, err)
		require.Len(t, content.Files, 2)
		assert.Equal(t, "lib/util.js", content.Files[0].Path)
		assert.Equal(t, "main.js", content.Files[1].Path)
	})

	t.Run("preserves subdirectories", func(t *testing.T) {
		fs := fstest.MapFS{
			"pkg/main.js":               &fstest.MapFile{Data: []byte(`main`)},
			"pkg/lib/state.js":          &fstest.MapFile{Data: []byte(`state`)},
			"pkg/lib/helpers/format.js": &fstest.MapFile{Data: []byte(`format`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "nested",
			Root:       "pkg",
			Entrypoint: "main.js",
		}

		content, err := LoadScriptPackage(pkg, fs)
		require.NoError(t, err)
		require.Len(t, content.Files, 3)
		assert.Equal(t, "lib/helpers/format.js", content.Files[0].Path)
		assert.Equal(t, "lib/state.js", content.Files[1].Path)
		assert.Equal(t, "main.js", content.Files[2].Path)
	})

	t.Run("files sorted", func(t *testing.T) {
		fs := fstest.MapFS{
			"pkg/z.js": &fstest.MapFile{Data: []byte(`z`)},
			"pkg/a.js": &fstest.MapFile{Data: []byte(`a`)},
			"pkg/m.js": &fstest.MapFile{Data: []byte(`m`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "sorted",
			Root:       "pkg",
			Entrypoint: "a.js",
		}

		content, err := LoadScriptPackage(pkg, fs)
		require.NoError(t, err)
		paths := make([]string, len(content.Files))
		for i, f := range content.Files {
			paths[i] = f.Path
		}
		assert.Equal(t, []string{"a.js", "m.js", "z.js"}, paths)
	})
}

// TestLoadScriptPackageInline verifies inline script packages: content is
// stored under InlineEntrypoint, and library packages (no entrypoint) load
// correctly.
func TestLoadScriptPackageInline(t *testing.T) {
	t.Run("runnable", func(t *testing.T) {
		script := `function main(ctx) { ctx.log("hello"); }`
		pkg := &v1alpha1.ScriptPackage{
			Name:   "inline-test",
			Inline: script,
		}

		content, err := LoadScriptPackage(pkg, nil)
		require.NoError(t, err)
		assert.Equal(t, "inline-test", content.Name)
		assert.Equal(t, InlineEntrypoint, content.Entrypoint)
		require.Len(t, content.Files, 1)
		assert.Equal(t, InlineEntrypoint, content.Files[0].Path)
		assert.Equal(t, script, string(content.Files[0].Content))
	})

	t.Run("library no entrypoint", func(t *testing.T) {
		fs := fstest.MapFS{
			"scripts/common/mcn.js":    &fstest.MapFile{Data: []byte(`exports.mcn = {}`)},
			"scripts/common/config.js": &fstest.MapFile{Data: []byte(`exports.config = {}`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name: "mco-common",
			Root: "scripts/common",
		}

		content, err := LoadScriptPackage(pkg, fs)
		require.NoError(t, err)
		assert.Equal(t, "mco-common", content.Name)
		assert.Equal(t, "", content.Entrypoint)
		require.Len(t, content.Files, 2)
	})
}

// TestLoadScriptPackageValidation verifies that invalid package definitions are
// rejected: path traversal, missing entrypoint, no matched files, missing root,
// missing source, and malformed globs.
func TestLoadScriptPackageValidation(t *testing.T) {
	t.Run("path traversal in root", func(t *testing.T) {
		pkg := &v1alpha1.ScriptPackage{
			Name:       "evil",
			Root:       "../outside",
			Entrypoint: "secret.js",
		}

		_, err := LoadScriptPackage(pkg, fstest.MapFS{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "traversal")
	})

	t.Run("missing entrypoint", func(t *testing.T) {
		fs := fstest.MapFS{
			"scripts/pkg/helper.js": &fstest.MapFile{Data: []byte(`helper`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "pkg",
			Root:       "scripts/pkg",
			Entrypoint: "main.js",
		}

		_, err := LoadScriptPackage(pkg, fs)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "entrypoint")
		assert.Contains(t, err.Error(), "main.js")
	})

	t.Run("no files matched", func(t *testing.T) {
		fs := fstest.MapFS{
			"scripts/pkg/data.json": &fstest.MapFile{Data: []byte(`{}`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "pkg",
			Root:       "scripts/pkg",
			Include:    []string{"*.js"},
			Entrypoint: "main.js",
		}

		_, err := LoadScriptPackage(pkg, fs)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no files matched")
	})

	t.Run("missing root", func(t *testing.T) {
		pkg := &v1alpha1.ScriptPackage{
			Name:       "missing",
			Root:       "nonexistent",
			Entrypoint: "main.js",
		}

		_, err := LoadScriptPackage(pkg, fstest.MapFS{})
		require.Error(t, err)
	})

	t.Run("no source", func(t *testing.T) {
		pkg := &v1alpha1.ScriptPackage{
			Name:       "empty",
			Entrypoint: "main.js",
		}

		_, err := LoadScriptPackage(pkg, fstest.MapFS{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "one of root or inline")
	})

	t.Run("malformed glob", func(t *testing.T) {
		fs := fstest.MapFS{
			"pkg/main.js": &fstest.MapFile{Data: []byte(`main`)},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:    "bad-glob",
			Root:    "pkg",
			Include: []string{"["},
		}

		_, err := LoadScriptPackage(pkg, fs)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid glob")
	})
}

// TestLoadScriptPackageSizeLimits verifies that packages exceeding file count,
// individual file size, package total size, and inline size limits are rejected.
func TestLoadScriptPackageSizeLimits(t *testing.T) {
	t.Run("file count", func(t *testing.T) {
		files := make(fstest.MapFS)
		for i := 0; i <= MaxScriptFiles; i++ {
			name := fmt.Sprintf("pkg/file%d.js", i)
			files[name] = &fstest.MapFile{Data: []byte(`x`)}
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "big",
			Root:       "pkg",
			Entrypoint: "file0.js",
		}

		_, err := LoadScriptPackage(pkg, files)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "maximum file count")
	})

	t.Run("file size", func(t *testing.T) {
		fs := fstest.MapFS{
			"pkg/huge.js": &fstest.MapFile{Data: []byte(strings.Repeat("x", MaxScriptFileSize+1))},
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "huge",
			Root:       "pkg",
			Entrypoint: "huge.js",
		}

		_, err := LoadScriptPackage(pkg, fs)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "maximum size")
	})

	t.Run("package total size", func(t *testing.T) {
		fs := make(fstest.MapFS)
		chunkSize := MaxScriptFileSize
		filesNeeded := (MaxPackageSize / chunkSize) + 1
		chunk := strings.Repeat("x", chunkSize)
		for i := 0; i < filesNeeded; i++ {
			name := fmt.Sprintf("pkg/file%d.js", i)
			fs[name] = &fstest.MapFile{Data: []byte(chunk)}
		}

		pkg := &v1alpha1.ScriptPackage{
			Name:       "fat",
			Root:       "pkg",
			Entrypoint: "file0.js",
		}

		_, err := LoadScriptPackage(pkg, fs)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "maximum total size")
	})

	t.Run("inline size", func(t *testing.T) {
		pkg := &v1alpha1.ScriptPackage{
			Name:   "huge-inline",
			Inline: strings.Repeat("x", MaxScriptFileSize+1),
		}

		_, err := LoadScriptPackage(pkg, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inline content exceeds maximum size")
	})
}

func TestLoadScriptPackageCapabilityValidation(t *testing.T) {
	makePkg := func(caps ...v1alpha1.ScriptCapability) *v1alpha1.ScriptPackage {
		return &v1alpha1.ScriptPackage{
			Name:         "pkg",
			Inline:       "code",
			Capabilities: caps,
		}
	}
	makeCap := func(scope v1alpha1.ScriptCapabilityScope, groups, resources, verbs []string) v1alpha1.ScriptCapability {
		return v1alpha1.ScriptCapability{
			Scope:      scope,
			PolicyRule: rbacv1.PolicyRule{APIGroups: groups, Resources: resources, Verbs: verbs},
		}
	}
	valid := makeCap(v1alpha1.ScopeCluster, []string{""}, []string{"nodes"}, []string{"get", "list"})

	accepted := []struct {
		name string
		cap  v1alpha1.ScriptCapability
	}{
		{"valid capability", valid},
		{"subresource via slash", makeCap(v1alpha1.ScopeCluster, []string{""}, []string{"nodes/status"}, []string{"patch"})},
		{"allNamespaces scope", makeCap(v1alpha1.ScopeAllNamespaces, []string{""}, []string{"pods"}, []string{"get"})},
		{"targetNamespace scope", makeCap(v1alpha1.ScopeTargetNamespace, []string{""}, []string{"pods"}, []string{"get"})},
	}
	for _, tc := range accepted {
		t.Run(tc.name+" accepted", func(t *testing.T) {
			_, err := LoadScriptPackage(makePkg(tc.cap), nil)
			require.NoError(t, err)
		})
	}

	t.Run("nil capabilities accepted", func(t *testing.T) {
		_, err := LoadScriptPackage(makePkg(), nil)
		require.NoError(t, err)
	})

	rejected := []struct {
		name     string
		cap      v1alpha1.ScriptCapability
		contains string
	}{
		{"invalid scope", makeCap("bogus", []string{""}, []string{"nodes"}, []string{"get"}), "unsupported scope"},
		{"empty scope", makeCap("", []string{""}, []string{"nodes"}, []string{"get"}), "unsupported scope"},
		{"resourceNames", v1alpha1.ScriptCapability{Scope: v1alpha1.ScopeCluster, PolicyRule: rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}, ResourceNames: []string{"foo"}}}, "resourceNames"},
		{"nonResourceURLs", v1alpha1.ScriptCapability{Scope: v1alpha1.ScopeCluster, PolicyRule: rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}, NonResourceURLs: []string{"/api"}}}, "nonResourceURLs"},
		{"empty apiGroups", makeCap(v1alpha1.ScopeCluster, nil, []string{"nodes"}, []string{"get"}), "apiGroup"},
		{"empty resources", makeCap(v1alpha1.ScopeCluster, []string{""}, nil, []string{"get"}), "resource"},
		{"empty verbs", makeCap(v1alpha1.ScopeCluster, []string{""}, []string{"nodes"}, nil), "verbs"},
		{"too many verbs", makeCap(v1alpha1.ScopeCluster, []string{""}, []string{"nodes"}, []string{"get", "list", "watch", "create", "update", "patch", "delete", "extra"}), "verbs"},
		{"wildcard apiGroup", makeCap(v1alpha1.ScopeCluster, []string{"*"}, []string{"nodes"}, []string{"get"}), "'*'"},
		{"wildcard resource", makeCap(v1alpha1.ScopeCluster, []string{""}, []string{"*"}, []string{"get"}), "'*'"},
		{"empty resource entry", makeCap(v1alpha1.ScopeCluster, []string{""}, []string{""}, []string{"get"}), "empty"},
		{"unsupported verb", makeCap(v1alpha1.ScopeCluster, []string{""}, []string{"nodes"}, []string{"escalate"}), "unsupported verb"},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := LoadScriptPackage(makePkg(tc.cap), nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.contains)
		})
	}
}
