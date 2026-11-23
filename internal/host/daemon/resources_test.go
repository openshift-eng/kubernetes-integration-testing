package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func mustUnstructured(t *testing.T, raw string) *unstructured.Unstructured {
	t.Helper()
	var obj unstructured.Unstructured
	require.NoError(t, yaml.Unmarshal([]byte(raw), &obj.Object))
	return &obj
}

func TestPrepareResources(t *testing.T) {
	t.Run("accepts kit CRs", func(t *testing.T) {
		objs, err := prepareResources([][]byte{
			[]byte("apiVersion: kit.openshift.io/v1alpha1\nkind: DeploymentPlan\nmetadata:\n  name: plan\nspec:\n  steps:\n  - name: s\n    collection:\n      source: c\n"),
			[]byte(`{"apiVersion":"kit.openshift.io/v1alpha1","kind":"ResourceCollection","metadata":{"name":"coll"},"spec":{}}`),
		})
		require.NoError(t, err)
		require.Len(t, objs, 2)
		assert.Equal(t, "DeploymentPlan", objs[0].GetKind())
		assert.Equal(t, "ResourceCollection", objs[1].GetKind())
	})

	t.Run("empty list", func(t *testing.T) {
		objs, err := prepareResources(nil)
		require.NoError(t, err)
		assert.Empty(t, objs)
	})

	rejections := []struct {
		name string
		raw  string
		want string
	}{
		{
			"KitCluster",
			"apiVersion: kit.openshift.io/v1alpha1\nkind: KitCluster\nmetadata:\n  name: cluster\nspec:\n  clusterName: x\n  provider: kwok\n",
			"cannot be passed as a resource",
		},
		{
			"CRD",
			"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: things.example.com\n",
			"CRDs cannot be passed as resources",
		},
		{
			"non-kit CR",
			"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: test\n",
			"not a kit CR",
		},
		{
			"missing apiVersion",
			"kind: DeploymentPlan\nmetadata:\n  name: a\n",
			"missing apiVersion",
		},
		{
			"missing kind",
			"apiVersion: kit.openshift.io/v1alpha1\nmetadata:\n  name: a\n",
			"missing kind",
		},
		{
			"missing name",
			"apiVersion: kit.openshift.io/v1alpha1\nkind: DeploymentPlan\n",
			"missing metadata.name",
		},
		{
			"garbage",
			"\tnot: [valid\n",
			"parsing resource",
		},
	}
	for _, tt := range rejections {
		t.Run(tt.name, func(t *testing.T) {
			_, err := prepareResources([][]byte{[]byte(tt.raw)})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestWaitConditionFor(t *testing.T) {
	tests := []struct {
		raw  string
		cond string
		ok   bool
	}{
		{"apiVersion: kit.openshift.io/v1alpha1\nkind: DeploymentPlan\nmetadata:\n  name: a\n", "Ready", true},
		{"apiVersion: kit.openshift.io/v1alpha1\nkind: ResourceCollection\nmetadata:\n  name: a\n", "Ready", true},
		{"apiVersion: kit.openshift.io/v1alpha1\nkind: WorkloadOverride\nmetadata:\n  name: a\n", "", false},
		{"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n", "", false},
	}
	for _, tt := range tests {
		obj := mustUnstructured(t, tt.raw)
		cond, ok := waitConditionFor(obj)
		assert.Equal(t, tt.ok, ok)
		assert.Equal(t, tt.cond, cond)
	}
}

func TestStatusConditions(t *testing.T) {
	obj := mustUnstructured(t, `apiVersion: kit.openshift.io/v1alpha1
kind: DeploymentPlan
metadata:
  name: plan
status:
  conditions:
  - type: Ready
    status: "True"
    reason: AllStepsReady
    message: all steps ready
`)
	conds := statusConditions(obj)
	require.Len(t, conds, 1)
	assert.Equal(t, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "AllStepsReady",
		Message: "all steps ready",
	}, conds[0])

	assert.Nil(t, statusConditions(mustUnstructured(t, "apiVersion: kit.openshift.io/v1alpha1\nkind: DeploymentPlan\nmetadata:\n  name: no-status\n")))
}
