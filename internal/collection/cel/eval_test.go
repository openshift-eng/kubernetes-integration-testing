package cel_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collcel "github.com/openshift-eng/kubernetes-integration-testing/internal/collection/cel"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func assertNativeTypes(t *testing.T, v interface{}, path string) {
	t.Helper()
	switch val := v.(type) {
	case nil, string, bool, int64, float64:
	case map[string]interface{}:
		for k, child := range val {
			assertNativeTypes(t, child, fmt.Sprintf("%s.%s", path, k))
		}
	case []interface{}:
		for i, child := range val {
			assertNativeTypes(t, child, fmt.Sprintf("%s[%d]", path, i))
		}
	default:
		t.Errorf("non-native type at %s: %s (%v)", path, reflect.TypeOf(v), v)
	}
}

func rawJSON(v interface{}) apiextensionsv1.JSON {
	b, _ := json.Marshal(v)
	return apiextensionsv1.JSON{Raw: b}
}

func TestEvalMapWithCELValues(t *testing.T) {
	ctx := collcel.Context{
		Cluster: collcel.ClusterContext{
			Platform:     "AWS",
			APIServerURL: "https://api.test:6443",
		},
	}
	patches := []collcel.StatusPatchInput{{
		Kind: "Infrastructure",
		Name: "cluster",
		Status: rawJSON(map[string]interface{}{
			"platform":             "cluster.platform",
			"apiServerInternalURI": "cluster.apiServerURL",
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
	assertNativeTypes(t, r.Status, "status")
}

// Two FeatureGate manifests with different feature-set annotations to exercise CEL filtering.
// In OCP FeatureGate is a singleton; here we duplicate it to test the query.
func TestEvalCELExpressionReturnsMap(t *testing.T) {
	ctx := collcel.Context{
		Cluster: collcel.ClusterContext{Platform: "None", FeatureSet: "Default"},
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
	}

	expr := `release.filter(m, m.kind == 'FeatureGate'
		&& m.metadata.name == 'cluster'
		&& m.metadata.annotations['release.openshift.io/feature-set'] == cluster.featureSet
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
	assertNativeTypes(t, results[0].Status, "status")
}

func TestEvalMapWithCELListAndIntValues(t *testing.T) {
	ctx := collcel.Context{
		Cluster: collcel.ClusterContext{
			Version: "4.18.0",
			Image:   "registry.example.com/release:4.18",
		},
	}
	patches := []collcel.StatusPatchInput{{
		Kind: "ClusterVersion",
		Name: "version",
		Status: rawJSON(map[string]interface{}{
			"desired": map[string]interface{}{
				"image":   "cluster.image",
				"version": "cluster.version",
			},
			"observedGeneration": "0",
			"versionHash":        "''",
			"availableUpdates":   "[]",
		}),
	}}

	results, err := collcel.EvalStatusPatches(ctx, patches)
	require.NoError(t, err)
	require.Len(t, results, 1)

	s := results[0].Status
	assertNativeTypes(t, s, "status")

	desired, ok := s["desired"].(map[string]interface{})
	require.True(t, ok, "desired should be a map, got %T", s["desired"])
	assert.Equal(t, "registry.example.com/release:4.18", desired["image"])
	assert.Equal(t, "4.18.0", desired["version"])
}

func TestAnnotationFunction(t *testing.T) {
	ctx := collcel.Context{
		Cluster: collcel.ClusterContext{FeatureSet: "TechPreview"},
		Release: []map[string]interface{}{
			{
				"kind": "FeatureGate",
				"metadata": map[string]interface{}{
					"name": "cluster",
					"annotations": map[string]interface{}{
						"release.openshift.io/feature-set": "TechPreview",
					},
				},
				"status": map[string]interface{}{"ready": true},
			},
			{
				"kind": "Infrastructure",
				"metadata": map[string]interface{}{
					"name": "cluster",
				},
				"status": map[string]interface{}{"platform": "AWS"},
			},
		},
	}

	patches := []collcel.StatusPatchInput{{
		Kind: "FeatureGate",
		Name: "cluster",
		Status: rawJSON(`release.filter(m,
			m.kind == 'FeatureGate'
			&& annotation(m, 'release.openshift.io/feature-set') == cluster.featureSet
		)[0].status`),
	}}

	results, err := collcel.EvalStatusPatches(ctx, patches)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, true, results[0].Status["ready"])
	assertNativeTypes(t, results[0].Status, "status")
}

func TestAnnotationMissing(t *testing.T) {
	ctx := collcel.Context{
		Release: []map[string]interface{}{
			{
				"kind":     "ConfigMap",
				"metadata": map[string]interface{}{"name": "test"},
			},
		},
	}

	patches := []collcel.StatusPatchInput{{
		Kind:   "ConfigMap",
		Name:   "test",
		Status: rawJSON(map[string]interface{}{"hasAnnotation": "annotation(release[0], 'missing-key') == ''"}),
	}}

	results, err := collcel.EvalStatusPatches(ctx, patches)
	require.NoError(t, err)
	assert.Equal(t, true, results[0].Status["hasAnnotation"])
}

func TestFindReleaseFunction(t *testing.T) {
	ctx := collcel.Context{
		Release: []map[string]interface{}{
			{
				"kind":     "Infrastructure",
				"metadata": map[string]interface{}{"name": "cluster"},
				"status":   map[string]interface{}{"platform": "AWS"},
			},
			{
				"kind":     "ClusterVersion",
				"metadata": map[string]interface{}{"name": "version"},
				"status":   map[string]interface{}{"desired": map[string]interface{}{"version": "4.18"}},
			},
		},
	}

	patches := []collcel.StatusPatchInput{{
		Kind:   "Infrastructure",
		Name:   "cluster",
		Status: rawJSON(`findRelease(release, 'Infrastructure', 'cluster').status`),
	}}

	results, err := collcel.EvalStatusPatches(ctx, patches)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "AWS", results[0].Status["platform"])
	assertNativeTypes(t, results[0].Status, "status")
}

func TestFindReleaseNotFound(t *testing.T) {
	ctx := collcel.Context{
		Release: []map[string]interface{}{
			{
				"kind":     "ConfigMap",
				"metadata": map[string]interface{}{"name": "test"},
			},
		},
	}

	patches := []collcel.StatusPatchInput{{
		Kind:   "Missing",
		Name:   "obj",
		Status: rawJSON(`findRelease(release, 'Missing', 'obj').status`),
	}}

	_, err := collcel.EvalStatusPatches(ctx, patches)
	assert.Error(t, err)
}

func TestFindReleaseByProfile(t *testing.T) {
	ctx := collcel.Context{
		Cluster: collcel.ClusterContext{FeatureSet: "Default"},
		Values:  map[string]any{"clusterProfile": "self-managed-high-availability"},
		Release: []map[string]interface{}{
			{
				"kind": "FeatureGate",
				"metadata": map[string]interface{}{
					"name": "cluster",
					"annotations": map[string]interface{}{
						"include.release.openshift.io/ibm-cloud-managed": "false-except-for-the-config-operator",
						"release.openshift.io/feature-set":               "Default",
					},
				},
				"status": map[string]interface{}{"featureGates": []interface{}{map[string]interface{}{"name": "gate-hyper"}}},
			},
			{
				"kind": "FeatureGate",
				"metadata": map[string]interface{}{
					"name": "cluster",
					"annotations": map[string]interface{}{
						"include.release.openshift.io/self-managed-high-availability": "false-except-for-the-config-operator",
						"release.openshift.io/feature-set":                            "Default",
					},
				},
				"status": map[string]interface{}{"featureGates": []interface{}{map[string]interface{}{"name": "gate-smha"}}},
			},
		},
	}

	patches := []collcel.StatusPatchInput{{
		Kind:   "FeatureGate",
		Name:   "cluster",
		Status: rawJSON(`findReleaseByProfile(release, 'FeatureGate', 'cluster', cluster.featureSet, values.clusterProfile).status`),
	}}

	results, err := collcel.EvalStatusPatches(ctx, patches)
	require.NoError(t, err)
	require.Len(t, results, 1)

	gates, ok := results[0].Status["featureGates"].([]interface{})
	require.True(t, ok)
	require.Len(t, gates, 1)
	gate := gates[0].(map[string]interface{})
	assert.Equal(t, "gate-smha", gate["name"], "should select self-managed-high-availability, not hypershift")
	assertNativeTypes(t, results[0].Status, "status")
}

func TestFindReleaseByProfileNotFound(t *testing.T) {
	ctx := collcel.Context{
		Cluster: collcel.ClusterContext{FeatureSet: "CustomNoUpgrade"},
		Values:  map[string]any{"clusterProfile": "self-managed-high-availability"},
		Release: []map[string]interface{}{
			{
				"kind": "FeatureGate",
				"metadata": map[string]interface{}{
					"name": "cluster",
					"annotations": map[string]interface{}{
						"include.release.openshift.io/self-managed-high-availability": "false-except-for-the-config-operator",
						"release.openshift.io/feature-set":                            "Default",
					},
				},
				"status": map[string]interface{}{},
			},
		},
	}

	patches := []collcel.StatusPatchInput{{
		Kind:   "FeatureGate",
		Name:   "cluster",
		Status: rawJSON(`findReleaseByProfile(release, 'FeatureGate', 'cluster', cluster.featureSet, values.clusterProfile).status`),
	}}

	_, err := collcel.EvalStatusPatches(ctx, patches)
	assert.Error(t, err, "should fail because no manifest matches CustomNoUpgrade")
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
