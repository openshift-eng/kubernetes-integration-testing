package cel_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collcel "github.com/openshift-eng/kubernetes-integration-testing/internal/collection/cel"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func rawJSON(v interface{}) apiextensionsv1.JSON {
	b, _ := json.Marshal(v)
	return apiextensionsv1.JSON{Raw: b}
}

func TestEvalMapWithCELValues(t *testing.T) {
	ctx := collcel.Context{
		Platform:     "AWS",
		APIServerURL: "https://api.test:6443",
	}
	patches := []collcel.StatusPatchInput{{
		Kind: "Infrastructure",
		Name: "cluster",
		Status: rawJSON(map[string]interface{}{
			"platform":            "platform",
			"apiServerInternalURI": "apiServerURL",
		}),
	}}

	results, err := collcel.EvalStatusPatches(ctx, patches)
	require.NoError(t, err)
	require.Len(t, results, 1)

	r := results[0]
	assert.Equal(t, "Infrastructure", r.Kind)
	assert.Equal(t, "cluster", r.Name)
	assert.Equal(t, "AWS", r.Status["platform"])
	assert.Equal(t, "https://api.test:6443", r.Status["apiServerInternalURI"])
}

// Two FeatureGate manifests with different feature-set annotations to exercise CEL filtering.
// In OCP FeatureGate is a singleton; here we duplicate it to test the query.
func TestEvalCELExpressionReturnsMap(t *testing.T) {
	ctx := collcel.Context{
		Platform: "None",
		Release: []map[string]interface{}{
			{
				"kind": "FeatureGate",
				"metadata": map[string]interface{}{
					"name": "cluster",
					"annotations": map[string]interface{}{
						"release.openshift.io/feature-set": "Default",
					},
				},
				"status": map[string]interface{}{
					"featureGates": []interface{}{
						map[string]interface{}{"name": "gate1", "enabled": true},
					},
				},
			},
			{
				"kind": "FeatureGate",
				"metadata": map[string]interface{}{
					"name": "cluster",
					"annotations": map[string]interface{}{
						"release.openshift.io/feature-set": "TechPreview",
					},
				},
				"status": map[string]interface{}{
					"featureGates": []interface{}{
						map[string]interface{}{"name": "gate2", "enabled": false},
					},
				},
			},
		},
		FeatureSet: "Default",
	}

	expr := `release.filter(m, m.kind == 'FeatureGate'
		&& m.metadata.name == 'cluster'
		&& m.metadata.annotations['release.openshift.io/feature-set'] == featureSet
	)[0].status`

	patches := []collcel.StatusPatchInput{{
		Kind:   "FeatureGate",
		Name:   "cluster",
		Status: rawJSON(expr),
	}}

	results, err := collcel.EvalStatusPatches(ctx, patches)
	require.NoError(t, err)
	require.Len(t, results, 1)

	gates, ok := results[0].Status["featureGates"].([]interface{})
	require.True(t, ok, "expected featureGates list")
	require.Len(t, gates, 1)

	gate := gates[0].(map[string]interface{})
	assert.Equal(t, "gate1", gate["name"], "should select Default featureSet variant")
}

func TestEvalLiteralStringNotCEL(t *testing.T) {
	ctx := collcel.Context{}
	patches := []collcel.StatusPatchInput{{
		Kind: "Test",
		Name: "obj",
		Status: rawJSON(map[string]interface{}{
			"message": "hello world",
		}),
	}}

	results, err := collcel.EvalStatusPatches(ctx, patches)
	require.NoError(t, err)
	assert.Equal(t, "hello world", results[0].Status["message"], "literal string should pass through")
}
