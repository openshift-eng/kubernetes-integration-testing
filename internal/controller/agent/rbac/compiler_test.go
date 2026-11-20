package rbac_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent/rbac"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testInput(caps []collection.AtomicCapability) rbac.CompileInput {
	return rbac.CompileInput{
		Name:            "kit-agent-mcd",
		TargetNamespace: "openshift-mco",
		OwnerRef: metav1.OwnerReference{
			APIVersion: "kit.openshift.io/v1alpha1",
			Kind:       "Agent",
			Name:       "mcd",
			UID:        types.UID("agent-uid"),
		},
		EffectiveCapabilities: caps,
	}
}

func TestCompileEmpty(t *testing.T) {
	objects := rbac.Compile(testInput(nil))
	assert.Empty(t, objects)
}

func TestCompileTargetNamespace(t *testing.T) {
	caps := []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "list"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "apps", Resource: "deployments", Verb: "get"},
	}
	objects := rbac.Compile(testInput(caps))
	require.Len(t, objects, 2)

	role, ok := objects[0].(*rbacv1.Role)
	require.True(t, ok)
	rb, ok := objects[1].(*rbacv1.RoleBinding)
	require.True(t, ok)

	t.Run("role metadata", func(t *testing.T) {
		assert.Equal(t, "kit-agent-mcd", role.Name)
		assert.Equal(t, "openshift-mco", role.Namespace)
		assert.Equal(t, "Role", role.Kind)
		assert.Equal(t, rbacv1.SchemeGroupVersion.String(), role.APIVersion)
		assert.Equal(t, "agent-uid", role.Labels[rbac.LabelAgentUID])
		require.Len(t, role.OwnerReferences, 1)
		assert.Equal(t, types.UID("agent-uid"), role.OwnerReferences[0].UID)
	})

	t.Run("role rules group by apiGroup+resource and union verbs", func(t *testing.T) {
		require.Len(t, role.Rules, 2)
		assert.Equal(t, rbacv1.PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list"},
		}, role.Rules[0])
		assert.Equal(t, rbacv1.PolicyRule{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments"},
			Verbs:     []string{"get"},
		}, role.Rules[1])
	})

	t.Run("binding", func(t *testing.T) {
		assert.Equal(t, "kit-agent-mcd", rb.Name)
		assert.Equal(t, "openshift-mco", rb.Namespace)
		assert.Equal(t, "RoleBinding", rb.Kind)
		require.Len(t, rb.Subjects, 1)
		assert.Equal(t, "ServiceAccount", rb.Subjects[0].Kind)
		assert.Equal(t, "kit-agent-mcd", rb.Subjects[0].Name)
		assert.Equal(t, "openshift-mco", rb.Subjects[0].Namespace)
		assert.Equal(t, rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     "kit-agent-mcd",
		}, rb.RoleRef)
	})

	t.Run("no cluster-scoped objects", func(t *testing.T) {
		for _, obj := range objects {
			assert.NotEqual(t, "ClusterRole", obj.GetObjectKind().GroupVersionKind().Kind)
			assert.NotEqual(t, "ClusterRoleBinding", obj.GetObjectKind().GroupVersionKind().Kind)
		}
	})
}

func TestCompileClusterScope(t *testing.T) {
	caps := []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeCluster, APIGroup: "", Resource: "nodes", Verb: "get"},
		{Scope: v1alpha1.ScopeCluster, APIGroup: "", Resource: "nodes", Verb: "list"},
	}
	objects := rbac.Compile(testInput(caps))
	require.Len(t, objects, 2)

	cr, ok := objects[0].(*rbacv1.ClusterRole)
	require.True(t, ok)
	crb, ok := objects[1].(*rbacv1.ClusterRoleBinding)
	require.True(t, ok)

	t.Run("cluster role metadata", func(t *testing.T) {
		assert.Equal(t, "kit-agent-mcd", cr.Name)
		assert.Empty(t, cr.Namespace)
		assert.Equal(t, "ClusterRole", cr.Kind)
		assert.Equal(t, "agent-uid", cr.Labels[rbac.LabelAgentUID])
	})

	t.Run("cluster role rules", func(t *testing.T) {
		require.Len(t, cr.Rules, 1)
		assert.Equal(t, rbacv1.PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list"},
		}, cr.Rules[0])
	})

	t.Run("cluster role binding", func(t *testing.T) {
		assert.Equal(t, "kit-agent-mcd", crb.Name)
		require.Len(t, crb.Subjects, 1)
		assert.Equal(t, "openshift-mco", crb.Subjects[0].Namespace)
		assert.Equal(t, rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "kit-agent-mcd",
		}, crb.RoleRef)
	})

	t.Run("no namespaced objects", func(t *testing.T) {
		for _, obj := range objects {
			assert.NotEqual(t, "Role", obj.GetObjectKind().GroupVersionKind().Kind)
			assert.NotEqual(t, "RoleBinding", obj.GetObjectKind().GroupVersionKind().Kind)
		}
	})
}

func TestCompileAllNamespaces(t *testing.T) {
	caps := []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeAllNamespaces, APIGroup: "", Resource: "pods", Verb: "get"},
	}
	objects := rbac.Compile(testInput(caps))
	require.Len(t, objects, 2)

	cr := objects[0].(*rbacv1.ClusterRole)
	require.Len(t, cr.Rules, 1)
	assert.Equal(t, []string{"pods"}, cr.Rules[0].Resources)
}

func TestCompileMixedScopes(t *testing.T) {
	caps := []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "get"},
		{Scope: v1alpha1.ScopeCluster, APIGroup: "", Resource: "nodes", Verb: "get"},
		{Scope: v1alpha1.ScopeAllNamespaces, APIGroup: "", Resource: "configmaps", Verb: "list"},
	}
	objects := rbac.Compile(testInput(caps))
	require.Len(t, objects, 4)

	role := objects[0].(*rbacv1.Role)
	assert.Len(t, role.Rules, 1)
	assert.Equal(t, []string{"pods"}, role.Rules[0].Resources)

	cr := objects[2].(*rbacv1.ClusterRole)
	assert.Len(t, cr.Rules, 2)
}

func TestCompileVerbDedup(t *testing.T) {
	caps := []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "list"},
	}
	objects := rbac.Compile(testInput(caps))
	role := objects[0].(*rbacv1.Role)
	require.Len(t, role.Rules, 1)
	assert.Equal(t, []string{"get", "list"}, role.Rules[0].Verbs)
}

func TestCompileRulesSorted(t *testing.T) {
	caps := []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "batch", Resource: "jobs", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "apps", Resource: "deployments", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "get"},
	}
	objects := rbac.Compile(testInput(caps))
	role := objects[0].(*rbacv1.Role)
	require.Len(t, role.Rules, 4)
	assert.Equal(t, []string{""}, role.Rules[0].APIGroups)
	assert.Equal(t, []string{"configmaps"}, role.Rules[0].Resources)
	assert.Equal(t, []string{""}, role.Rules[1].APIGroups)
	assert.Equal(t, []string{"pods"}, role.Rules[1].Resources)
	assert.Equal(t, []string{"apps"}, role.Rules[2].APIGroups)
	assert.Equal(t, []string{"batch"}, role.Rules[3].APIGroups)
}

func TestCompileNamespaceChange(t *testing.T) {
	caps := []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "get"},
		{Scope: v1alpha1.ScopeCluster, APIGroup: "", Resource: "nodes", Verb: "get"},
	}
	input := testInput(caps)
	input.TargetNamespace = "new-namespace"
	objects := rbac.Compile(input)
	require.Len(t, objects, 4)

	role := objects[0].(*rbacv1.Role)
	rb := objects[1].(*rbacv1.RoleBinding)
	crb := objects[3].(*rbacv1.ClusterRoleBinding)

	assert.Equal(t, "new-namespace", role.Namespace)
	assert.Equal(t, "new-namespace", rb.Namespace)
	assert.Equal(t, "new-namespace", rb.Subjects[0].Namespace)
	assert.Equal(t, "new-namespace", crb.Subjects[0].Namespace)
}
