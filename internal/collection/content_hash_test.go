package collection

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func hex64(prefix string) string {
	return (prefix + strings.Repeat("0", 64))[:64]
}

func TestCombineContentHash(t *testing.T) {
	h1 := hex64("aa")
	h2 := hex64("bb")
	h3 := hex64("cc")
	h4 := hex64("dd")

	t.Run("deterministic", func(t *testing.T) {
		c := ContentHashComponents{
			RenderHash:        h1,
			ScriptHash:        h2,
			StatusPatchesHash: h3,
		}
		got1, err1 := CombineContentHash(c)
		require.NoError(t, err1)
		got2, err2 := CombineContentHash(c)
		require.NoError(t, err2)
		assert.Equal(t, got1, got2)
	})

	t.Run("changes with render", func(t *testing.T) {
		base, err := CombineContentHash(ContentHashComponents{RenderHash: h1, ScriptHash: h2, StatusPatchesHash: h3})
		require.NoError(t, err)
		modified, err := CombineContentHash(ContentHashComponents{RenderHash: h4, ScriptHash: h2, StatusPatchesHash: h3})
		require.NoError(t, err)
		assert.NotEqual(t, base, modified)
	})

	t.Run("changes with scripts", func(t *testing.T) {
		base, err := CombineContentHash(ContentHashComponents{RenderHash: h1, ScriptHash: h2, StatusPatchesHash: h3})
		require.NoError(t, err)
		modified, err := CombineContentHash(ContentHashComponents{RenderHash: h1, ScriptHash: h4, StatusPatchesHash: h3})
		require.NoError(t, err)
		assert.NotEqual(t, base, modified)
	})

	t.Run("changes with statusPatches", func(t *testing.T) {
		base, err := CombineContentHash(ContentHashComponents{RenderHash: h1, ScriptHash: h2, StatusPatchesHash: h3})
		require.NoError(t, err)
		modified, err := CombineContentHash(ContentHashComponents{RenderHash: h1, ScriptHash: h2, StatusPatchesHash: h4})
		require.NoError(t, err)
		assert.NotEqual(t, base, modified)
	})

	t.Run("rejects invalid input", func(t *testing.T) {
		_, err := CombineContentHash(ContentHashComponents{RenderHash: "short", ScriptHash: h2, StatusPatchesHash: h3})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "RenderHash")
	})

	t.Run("rejects uppercase hex", func(t *testing.T) {
		upper := strings.ToUpper(h1)
		_, err := CombineContentHash(ContentHashComponents{RenderHash: upper, ScriptHash: h2, StatusPatchesHash: h3})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "lowercase")
	})
}

func TestHashStatusPatches(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		patches := []v1alpha1.StatusPatch{
			{Kind: "Node", Name: "node-1", Status: apiextensionsv1.JSON{Raw: []byte(`{"ready":true}`)}},
		}
		h1, err := HashStatusPatches(patches)
		require.NoError(t, err)
		h2, err := HashStatusPatches(patches)
		require.NoError(t, err)
		assert.Equal(t, h1, h2)
	})

	t.Run("changes with content", func(t *testing.T) {
		p1 := []v1alpha1.StatusPatch{
			{Kind: "Node", Name: "node-1", Status: apiextensionsv1.JSON{Raw: []byte(`{"ready":true}`)}},
		}
		p2 := []v1alpha1.StatusPatch{
			{Kind: "Node", Name: "node-1", Status: apiextensionsv1.JSON{Raw: []byte(`{"ready":false}`)}},
		}
		h1, err := HashStatusPatches(p1)
		require.NoError(t, err)
		h2, err := HashStatusPatches(p2)
		require.NoError(t, err)
		assert.NotEqual(t, h1, h2)
	})

	t.Run("order matters", func(t *testing.T) {
		a := v1alpha1.StatusPatch{Kind: "Node", Name: "a", Status: apiextensionsv1.JSON{Raw: []byte(`{}`)}}
		b := v1alpha1.StatusPatch{Kind: "Node", Name: "b", Status: apiextensionsv1.JSON{Raw: []byte(`{}`)}}

		h1, err := HashStatusPatches([]v1alpha1.StatusPatch{a, b})
		require.NoError(t, err)
		h2, err := HashStatusPatches([]v1alpha1.StatusPatch{b, a})
		require.NoError(t, err)
		assert.NotEqual(t, h1, h2)
	})

	t.Run("ignores JSON key order", func(t *testing.T) {
		p1 := []v1alpha1.StatusPatch{
			{Kind: "Node", Name: "n", Status: apiextensionsv1.JSON{Raw: []byte(`{"a":1,"b":2}`)}},
		}
		p2 := []v1alpha1.StatusPatch{
			{Kind: "Node", Name: "n", Status: apiextensionsv1.JSON{Raw: []byte(`{"b":2,"a":1}`)}},
		}
		h1, err := HashStatusPatches(p1)
		require.NoError(t, err)
		h2, err := HashStatusPatches(p2)
		require.NoError(t, err)
		assert.Equal(t, h1, h2)
	})

	t.Run("empty patches", func(t *testing.T) {
		h, err := HashStatusPatches(nil)
		require.NoError(t, err)
		assert.NotEmpty(t, h)
	})
}
