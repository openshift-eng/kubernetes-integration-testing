package cel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collcel "github.com/openshift-eng/kubernetes-integration-testing/internal/collection/cel"
)

func evalMergeExpr(t *testing.T, expr string) map[string]interface{} {
	t.Helper()
	patches := []collcel.StatusPatchInput{{
		Kind:   "Test",
		Name:   "obj",
		Status: rawJSON(expr),
	}}
	results, err := collcel.EvalStatusPatches(collcel.Context{}, patches)
	require.NoError(t, err)
	return results[0].Status
}

func TestMergePrimitiveOverride(t *testing.T) {
	r := evalMergeExpr(t, `{"a": 1, "b": "old"}.merge({"b": "new", "c": true})`)

	assert.Equal(t, int64(1), r["a"], "base key preserved")
	assert.Equal(t, "new", r["b"], "overlay wins")
	assert.Equal(t, true, r["c"], "new key added")
}

func TestMergeNestedMaps(t *testing.T) {
	r := evalMergeExpr(t, `{"a": {"x": 1, "y": 2}}.merge({"a": {"y": 3, "z": 4}})`)

	a, ok := r["a"].(map[string]interface{})
	require.True(t, ok, "expected nested map")
	assert.Equal(t, int64(1), a["x"], "base nested key preserved")
	assert.Equal(t, int64(3), a["y"], "overlay nested key wins")
	assert.Equal(t, int64(4), a["z"], "new nested key added")
}

func TestMergeDeeplyNested(t *testing.T) {
	r := evalMergeExpr(t, `{"l1": {"l2": {"l3": "base", "keep": true}}}.merge({"l1": {"l2": {"l3": "overlay"}}})`)

	l1 := r["l1"].(map[string]interface{})
	l2 := l1["l2"].(map[string]interface{})
	assert.Equal(t, "overlay", l2["l3"], "deep override")
	assert.Equal(t, true, l2["keep"], "deep preserve")
}

func TestMergeSliceReplaces(t *testing.T) {
	r := evalMergeExpr(t, `{"items": [1, 2, 3]}.merge({"items": [4, 5]})`)

	items, ok := r["items"].([]interface{})
	require.True(t, ok, "expected slice")
	assert.Equal(t, []interface{}{int64(4), int64(5)}, items, "overlay slice replaces")
}

func TestMergeSliceOfMapsReplaces(t *testing.T) {
	r := evalMergeExpr(t, `{"gates": [{"name": "a"}]}.merge({"gates": [{"name": "b"}, {"name": "c"}]})`)

	gates := r["gates"].([]interface{})
	require.Len(t, gates, 2)
	assert.Equal(t, "b", gates[0].(map[string]interface{})["name"])
	assert.Equal(t, "c", gates[1].(map[string]interface{})["name"])
}

func TestMergeNestedMapWithSlicePreserved(t *testing.T) {
	r := evalMergeExpr(t, `
		{"status": {"gates": [{"name": "a"}], "version": "old"}}.merge(
		{"status": {"version": "new"}})
	`)

	status, ok := r["status"].(map[string]interface{})
	require.True(t, ok, "expected nested map")
	assert.Equal(t, "new", status["version"], "nested primitive overridden")

	gates, ok := status["gates"].([]interface{})
	require.True(t, ok, "slice inside merged map preserved")
	require.Len(t, gates, 1)
	assert.Equal(t, "a", gates[0].(map[string]interface{})["name"])
}

func TestMergeNestedMapSliceReplaced(t *testing.T) {
	r := evalMergeExpr(t, `
		{"status": {"gates": [{"name": "a"}], "keep": true}}.merge(
		{"status": {"gates": [{"name": "b"}, {"name": "c"}]}})
	`)

	status := r["status"].(map[string]interface{})
	assert.Equal(t, true, status["keep"], "sibling key preserved")

	gates := status["gates"].([]interface{})
	require.Len(t, gates, 2, "nested slice replaced, not appended")
	assert.Equal(t, "b", gates[0].(map[string]interface{})["name"])
	assert.Equal(t, "c", gates[1].(map[string]interface{})["name"])
}

func TestMergeEmptyOverlay(t *testing.T) {
	r := evalMergeExpr(t, `{"a": 1, "b": 2}.merge({})`)

	assert.Equal(t, int64(1), r["a"])
	assert.Equal(t, int64(2), r["b"])
}

func TestMergeEmptyBase(t *testing.T) {
	r := evalMergeExpr(t, `{}.merge({"a": 1})`)

	assert.Equal(t, int64(1), r["a"])
}

func TestMergeTypeMismatchOverlayWins(t *testing.T) {
	r := evalMergeExpr(t, `{"a": {"nested": true}}.merge({"a": "flat"})`)

	assert.Equal(t, "flat", r["a"], "overlay type wins over base")
}

func TestMergeWithContextVariables(t *testing.T) {
	patches := []collcel.StatusPatchInput{{
		Kind:   "FeatureGate",
		Name:   "cluster",
		Status: rawJSON(`release.filter(m, m.kind == 'FeatureGate')[0].status.merge({"version": cluster.version})`),
	}}

	results, err := collcel.EvalStatusPatches(collcel.Context{
		Cluster: collcel.ClusterContext{Version: "4.18.0"},
		Release: []map[string]interface{}{
			{
				"kind":     "FeatureGate",
				"metadata": map[string]interface{}{"name": "cluster"},
				"status": map[string]interface{}{
					"featureGates": []interface{}{
						map[string]interface{}{"name": "gate1", "enabled": true},
					},
				},
			},
		},
	}, patches)
	require.NoError(t, err)

	r := results[0]
	assert.Equal(t, "4.18.0", r.Status["version"], "version injected")

	gates, ok := r.Status["featureGates"].([]interface{})
	require.True(t, ok, "featureGates preserved")
	require.Len(t, gates, 1)
	assert.Equal(t, "gate1", gates[0].(map[string]interface{})["name"], "gate content preserved")
}
