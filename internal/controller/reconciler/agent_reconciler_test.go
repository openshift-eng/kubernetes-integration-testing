package reconciler_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
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

type fakeScriptLoader struct {
	calledWith []string
}

func (f *fakeScriptLoader) Load(_ context.Context, collectionName string, _ collection.ScriptCollectionResolver) (*collection.LoadResult, error) {
	f.calledWith = append(f.calledWith, collectionName)
	return &collection.LoadResult{
		Packages: []collection.ScriptPackageContent{{
			Name:       "test-pkg",
			Entrypoint: "main.js",
			Files: []collection.ScriptFile{
				{Path: "main.js", Content: []byte(`exports.main = function(ctx) {};`)},
			},
		}},
		LocalPackages: map[string]bool{"test-pkg": true},
		EffectiveCapabilities: []collection.AtomicCapability{
			{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "get"},
			{Scope: v1alpha1.ScopeCluster, APIGroup: "", Resource: "nodes", Verb: "get"},
		},
		ContentHash: "fake",
	}, nil
}

func agentTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, rbacv1.AddToScheme(s))
	return s
}

func agentTestSetup(t *testing.T, objects ...client.Object) (client.WithWatch, *agentctl.Materializer, *fakeScriptLoader) {
	t.Helper()

	c := fake.NewClientBuilder().
		WithScheme(agentTestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.Agent{}).
		Build()

	store, err := bundle.NewBundleStore(afero.NewMemMapFs(), "/bundles", slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	loader := &fakeScriptLoader{}
	mat := agentctl.NewMaterializer(c, store, loader, "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))
	return c, mat, loader
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
		c, mat, _ := agentTestSetup(t, ag)

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
		c, mat, loader := agentTestSetup(t, ag, ns)

		reconcileAgent(t, c, mat, "mcd-stub")

		require.Len(t, loader.calledWith, 1)
		assert.Equal(t, "test-collection", loader.calledWith[0])

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
		c, mat, _ := agentTestSetup(t, ag, ns)

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

		var cr rbacv1.ClusterRole
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{
			Name: "kit-agent-mcd-stub",
		}, &cr))

		var crb rbacv1.ClusterRoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{
			Name: "kit-agent-mcd-stub",
		}, &crb))

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &wo))
	})

	t.Run("deleted agent is ignored", func(t *testing.T) {
		c, mat, _ := agentTestSetup(t)
		reconcileAgent(t, c, mat, "nonexistent")
	})

	t.Run("namespace appears triggers ready", func(t *testing.T) {
		ag := testAgentCR()
		c, mat, _ := agentTestSetup(t, ag)

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
