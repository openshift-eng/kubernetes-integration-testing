package reconciler_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	agentctl "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func agentTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, rbacv1.AddToScheme(s))
	return s
}

func agentTestSetup(t *testing.T, objects ...client.Object) (client.WithWatch, *agentctl.Materializer, string) {
	t.Helper()

	collectionsDir := t.TempDir()
	collDir := filepath.Join(collectionsDir, "test-collection", "scripts", "test-pkg")
	require.NoError(t, os.MkdirAll(collDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(collDir, "main.js"), []byte(`exports.main = function(ctx) {};`), 0644))

	c := fake.NewClientBuilder().
		WithScheme(agentTestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.Agent{}).
		Build()

	store, err := bundle.NewBundleStore(afero.NewMemMapFs(), "/bundles", slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	mat := agentctl.NewMaterializer(c, store, collectionsDir, "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))
	return c, mat, collectionsDir
}

func testAgentCR() *v1alpha1.Agent {
	return &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "mcd-stub",
			UID:        types.UID("agent-uid"),
			Generation: 1,
		},
		Spec: v1alpha1.AgentSpec{
			Target: v1alpha1.AgentTarget{
				Namespace: "openshift-machine-config-operator",
				Replace:   v1alpha1.AgentReplacePod,
				Labels:    map[string]string{"app": "mcd"},
			},
			Runtime: v1alpha1.AgentRuntimeJS,
			Script: v1alpha1.AgentScriptRef{
				Collection: "test-collection",
				Package:    "test-pkg",
			},
		},
	}
}

func testResourceCollection() *v1alpha1.ResourceCollection {
	return &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{Name: "test-collection"},
		Spec: v1alpha1.ResourceCollectionSpec{
			Source: "test-collection",
			Scripts: []v1alpha1.ScriptPackage{{
				Name:       "test-pkg",
				Root:       "scripts/test-pkg",
				Entrypoint: "main.js",
			}},
		},
	}
}

func reconcileAgent(t *testing.T, c client.Client, mat *agentctl.Materializer, name string) ctrl.Result {
	t.Helper()
	r := reconciler.NewAgentReconciler(c, mat, slog.New(slog.DiscardHandler))
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	require.NoError(t, err)
	return result
}

func TestAgentReconciler(t *testing.T) {
	t.Run("namespace pending when target does not exist", func(t *testing.T) {
		ag := testAgentCR()
		c, mat, _ := agentTestSetup(t, ag, testResourceCollection())

		reconcileAgent(t, c, mat, "mcd-stub")

		var updated v1alpha1.Agent
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "mcd-stub"}, &updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionAgentReady)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, v1alpha1.ReasonAgentNamespacePending, cond.Reason)
		assert.Empty(t, updated.Status.BundleDigest)
	})

	t.Run("ready when namespace exists", func(t *testing.T) {
		ag := testAgentCR()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-machine-config-operator"}}
		c, mat, _ := agentTestSetup(t, ag, testResourceCollection(), ns)

		reconcileAgent(t, c, mat, "mcd-stub")

		var updated v1alpha1.Agent
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "mcd-stub"}, &updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionAgentReady)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, v1alpha1.ReasonAgentReady, cond.Reason)
		assert.NotEmpty(t, updated.Status.BundleDigest)
		assert.Equal(t, int64(1), updated.Status.ObservedGeneration)
	})

	t.Run("materializer creates all generated resources", func(t *testing.T) {
		ag := testAgentCR()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-machine-config-operator"}}
		c, mat, _ := agentTestSetup(t, ag, testResourceCollection(), ns)

		reconcileAgent(t, c, mat, "mcd-stub")

		var sa corev1.ServiceAccount
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{
			Name: "kit-agent-mcd-stub", Namespace: "openshift-machine-config-operator",
		}, &sa))

		var role rbacv1.Role
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{
			Name: "kit-agent-mcd-stub", Namespace: "openshift-machine-config-operator",
		}, &role))

		var rb rbacv1.RoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{
			Name: "kit-agent-mcd-stub", Namespace: "openshift-machine-config-operator",
		}, &rb))

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &wo))
	})

	t.Run("deleted agent is ignored", func(t *testing.T) {
		c, mat, _ := agentTestSetup(t)
		reconcileAgent(t, c, mat, "nonexistent")
	})

	t.Run("namespace appears triggers ready", func(t *testing.T) {
		ag := testAgentCR()
		c, mat, _ := agentTestSetup(t, ag, testResourceCollection())

		reconcileAgent(t, c, mat, "mcd-stub")
		var updated v1alpha1.Agent
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "mcd-stub"}, &updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionAgentReady)
		require.NotNil(t, cond)
		assert.Equal(t, v1alpha1.ReasonAgentNamespacePending, cond.Reason)

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-machine-config-operator"}}
		require.NoError(t, c.Create(context.Background(), ns))

		reconcileAgent(t, c, mat, "mcd-stub")
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "mcd-stub"}, &updated))
		cond = meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionAgentReady)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
	})
}
