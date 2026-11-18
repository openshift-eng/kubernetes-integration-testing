package rbac_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent/rbac"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testInput() rbac.CompileInput {
	return rbac.CompileInput{
		Name:            "kit-agent-mcd",
		TargetNamespace: "openshift-mco",
		OwnerRef: metav1.OwnerReference{
			APIVersion: "kit.openshift.io/v1alpha1",
			Kind:       "Agent",
			Name:       "mcd",
			UID:        types.UID("agent-uid"),
		},
	}
}

func TestCompile(t *testing.T) {
	objects := rbac.Compile(testInput())
	require.Len(t, objects, 4)

	role, ok := objects[0].(*rbacv1.Role)
	require.True(t, ok, "first object should be a Role")
	rb, ok := objects[1].(*rbacv1.RoleBinding)
	require.True(t, ok, "second object should be a RoleBinding")
	cr, ok := objects[2].(*rbacv1.ClusterRole)
	require.True(t, ok, "third object should be a ClusterRole")
	crb, ok := objects[3].(*rbacv1.ClusterRoleBinding)
	require.True(t, ok, "fourth object should be a ClusterRoleBinding")

	t.Run("names and namespaces", func(t *testing.T) {
		assert.Equal(t, "kit-agent-mcd", role.Name)
		assert.Equal(t, "openshift-mco", role.Namespace)

		assert.Equal(t, "kit-agent-mcd", rb.Name)
		assert.Equal(t, "openshift-mco", rb.Namespace)

		assert.Equal(t, "kit-agent-mcd", cr.Name)
		assert.Empty(t, cr.Namespace)

		assert.Equal(t, "kit-agent-mcd", crb.Name)
		assert.Empty(t, crb.Namespace)
	})

	t.Run("labels", func(t *testing.T) {
		for _, obj := range objects {
			labels := obj.GetLabels()
			assert.Equal(t, "agent-uid", labels[rbac.LabelAgentUID], "object %T missing agent-uid label", obj)
		}
	})

	t.Run("owner references", func(t *testing.T) {
		for _, obj := range objects {
			refs := obj.GetOwnerReferences()
			require.Len(t, refs, 1, "object %T should have one owner ref", obj)
			assert.Equal(t, "Agent", refs[0].Kind)
			assert.Equal(t, "mcd", refs[0].Name)
			assert.Equal(t, types.UID("agent-uid"), refs[0].UID)
		}
	})

	t.Run("TypeMeta set for SSA", func(t *testing.T) {
		assert.Equal(t, "Role", role.Kind)
		assert.Equal(t, rbacv1.SchemeGroupVersion.String(), role.APIVersion)

		assert.Equal(t, "RoleBinding", rb.Kind)
		assert.Equal(t, rbacv1.SchemeGroupVersion.String(), rb.APIVersion)

		assert.Equal(t, "ClusterRole", cr.Kind)
		assert.Equal(t, rbacv1.SchemeGroupVersion.String(), cr.APIVersion)

		assert.Equal(t, "ClusterRoleBinding", crb.Kind)
		assert.Equal(t, rbacv1.SchemeGroupVersion.String(), crb.APIVersion)
	})

	t.Run("binding subjects", func(t *testing.T) {
		require.Len(t, rb.Subjects, 1)
		assert.Equal(t, "ServiceAccount", rb.Subjects[0].Kind)
		assert.Equal(t, "kit-agent-mcd", rb.Subjects[0].Name)
		assert.Equal(t, "openshift-mco", rb.Subjects[0].Namespace)

		require.Len(t, crb.Subjects, 1)
		assert.Equal(t, "ServiceAccount", crb.Subjects[0].Kind)
		assert.Equal(t, "kit-agent-mcd", crb.Subjects[0].Name)
		assert.Equal(t, "openshift-mco", crb.Subjects[0].Namespace)
	})

	t.Run("role refs", func(t *testing.T) {
		assert.Equal(t, "Role", rb.RoleRef.Kind)
		assert.Equal(t, "kit-agent-mcd", rb.RoleRef.Name)
		assert.Equal(t, rbacv1.GroupName, rb.RoleRef.APIGroup)

		assert.Equal(t, "ClusterRole", crb.RoleRef.Kind)
		assert.Equal(t, "kit-agent-mcd", crb.RoleRef.Name)
		assert.Equal(t, rbacv1.GroupName, crb.RoleRef.APIGroup)
	})

	t.Run("role has namespaced rules", func(t *testing.T) {
		require.NotEmpty(t, role.Rules)
		hasResource := func(group, resource string) bool {
			for _, rule := range role.Rules {
				for _, g := range rule.APIGroups {
					if g != group {
						continue
					}
					for _, r := range rule.Resources {
						if r == resource {
							return true
						}
					}
				}
			}
			return false
		}
		assert.True(t, hasResource("", "pods"))
		assert.True(t, hasResource("", "configmaps"))
		assert.True(t, hasResource("apps", "*"))
		assert.True(t, hasResource("batch", "*"))
		assert.False(t, hasResource("", "secrets"))
	})

	t.Run("cluster role has cluster-scoped rules", func(t *testing.T) {
		require.NotEmpty(t, cr.Rules)
		hasResource := func(group, resource string) bool {
			for _, rule := range cr.Rules {
				for _, g := range rule.APIGroups {
					if g != group {
						continue
					}
					for _, r := range rule.Resources {
						if r == resource || r == "*" {
							return true
						}
					}
				}
			}
			return false
		}
		assert.True(t, hasResource("", "nodes"))
		assert.True(t, hasResource("kit.openshift.io", "*"))
		assert.True(t, hasResource("machineconfiguration.openshift.io", "machineconfignodes"))
	})
}

func TestCompileNamespaceChange(t *testing.T) {
	input := testInput()
	input.TargetNamespace = "new-namespace"
	objects := rbac.Compile(input)

	role := objects[0].(*rbacv1.Role)
	rb := objects[1].(*rbacv1.RoleBinding)
	crb := objects[3].(*rbacv1.ClusterRoleBinding)

	assert.Equal(t, "new-namespace", role.Namespace)
	assert.Equal(t, "new-namespace", rb.Namespace)
	assert.Equal(t, "new-namespace", rb.Subjects[0].Namespace)
	assert.Equal(t, "new-namespace", crb.Subjects[0].Namespace)
}
