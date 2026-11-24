package client

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const multiDocYAML = `
apiVersion: kit.openshift.io/v1alpha1
kind: DeploymentPlan
metadata:
  name: plan-a
spec:
  steps:
  - name: step
    collection:
      source: some-collection
---
apiVersion: kit.openshift.io/v1alpha1
kind: ResourceCollection
metadata:
  name: coll-a
spec: {}
`

func TestFromStringMultiDoc(t *testing.T) {
	ms, err := FromString(multiDocYAML).Load()
	require.NoError(t, err)
	require.Len(t, ms, 2)

	assert.Equal(t, "DeploymentPlan", ms[0].Kind)
	assert.Equal(t, "plan-a", ms[0].Name)
	assert.Equal(t, "ResourceCollection", ms[1].Kind)
	assert.Equal(t, "coll-a", ms[1].Name)

	// Each manifest is a single self-contained JSON document.
	assert.JSONEq(t, `{"apiVersion":"kit.openshift.io/v1alpha1","kind":"ResourceCollection","metadata":{"name":"coll-a"},"spec":{}}`, string(ms[1].JSON))
}

func TestFromStringJSON(t *testing.T) {
	ms, err := FromString(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"test"}}`).Load()
	require.NoError(t, err)
	require.Len(t, ms, 1)
	assert.Equal(t, "Namespace", ms[0].Kind)
}

func TestFromStringSkipsEmptyDocuments(t *testing.T) {
	raw := "---\n# just a comment\n---\n" + multiDocYAML + "\n---\n\n"
	ms, err := FromString(raw).Load()
	require.NoError(t, err)
	assert.Len(t, ms, 2)
}

func TestFromStringValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"missing apiVersion", "kind: DeploymentPlan\nmetadata:\n  name: a\n", "missing apiVersion"},
		{"missing kind", "apiVersion: kit.openshift.io/v1alpha1\nmetadata:\n  name: a\n", "missing kind"},
		{"missing name", "apiVersion: kit.openshift.io/v1alpha1\nkind: DeploymentPlan\n", "missing metadata.name"},
		{"garbage", "\tnot: [valid: yaml\n", "parsing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromString(tt.raw).Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestFromBytes(t *testing.T) {
	ms, err := FromBytes(
		[]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n"),
		[]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"b"}}`),
	).Load()
	require.NoError(t, err)
	require.Len(t, ms, 2)
	assert.Equal(t, "a", ms[0].Name)
	assert.Equal(t, "b", ms[1].Name)
}

func TestFromFiles(t *testing.T) {
	dir := t.TempDir()

	// A directory with manifests, loaded in lexical order.
	resDir := filepath.Join(dir, "resources")
	require.NoError(t, os.MkdirAll(resDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(resDir, "b.yaml"), []byte("apiVersion: kit.openshift.io/v1alpha1\nkind: ResourceCollection\nmetadata:\n  name: from-dir-b\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(resDir, "a.yaml"), []byte("apiVersion: kit.openshift.io/v1alpha1\nkind: ResourceCollection\nmetadata:\n  name: from-dir-a\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(resDir, "notes.txt"), []byte("ignored"), 0o644))

	// A standalone file with two documents.
	planFile := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(planFile, []byte(multiDocYAML), 0o644))

	ms, err := FromFiles(planFile, resDir).Load()
	require.NoError(t, err)
	require.Len(t, ms, 4)

	// Files keep the order given; directory entries are sorted.
	assert.Equal(t, "plan-a", ms[0].Name)
	assert.Equal(t, "coll-a", ms[1].Name)
	assert.Equal(t, "from-dir-a", ms[2].Name)
	assert.Equal(t, "from-dir-b", ms[3].Name)
}

func TestFromFilesMissing(t *testing.T) {
	_, err := FromFiles(filepath.Join(t.TempDir(), "nope.yaml")).Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope.yaml")
}

func TestFromFS(t *testing.T) {
	fsys := fstest.MapFS{
		"manifests/b.yaml": {Data: []byte("apiVersion: kit.openshift.io/v1alpha1\nkind: ResourceCollection\nmetadata:\n  name: b\n")},
		"manifests/a.yaml": {Data: []byte("apiVersion: kit.openshift.io/v1alpha1\nkind: ResourceCollection\nmetadata:\n  name: a\n")},
		"manifests/sub":    {Mode: os.ModeDir | 0o755},
	}

	ms, err := FromFS(fsys, "manifests/*.yaml").Load()
	require.NoError(t, err)
	require.Len(t, ms, 2)
	assert.Equal(t, "a", ms[0].Name)
	assert.Equal(t, "b", ms[1].Name)
}

func TestFromFilesStdin(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	oldStdin := os.Stdin
	t.Cleanup(func() { os.Stdin = oldStdin })
	os.Stdin = r

	go func() {
		_, _ = w.Write([]byte("apiVersion: kit.openshift.io/v1alpha1\nkind: ResourceCollection\nmetadata:\n  name: from-stdin\n"))
		_ = w.Close()
	}()

	ms, err := FromFiles("-").Load()
	require.NoError(t, err)
	require.Len(t, ms, 1)
	assert.Equal(t, "from-stdin", ms[0].Name)
}
