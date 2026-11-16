// TODO: These tests only cover scriptCollections finalizer protection. Missing
// coverage for the rest of the collection reconciler: rendering (source, inline,
// values, funcmap), apply (SSA, namespace ordering), status patching (CEL
// evaluation, target resolution), content-hash skip, KitCluster readiness
// gating, DeploymentPlan request filtering, filesystem watcher, and auto-creation
// of builtin/on-disk collections.
package reconciler_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type stubScriptLoader struct{}

func (s *stubScriptLoader) Load(_ context.Context, _ string, _ collection.ScriptCollectionResolver) (*collection.LoadResult, error) {
	return &collection.LoadResult{ContentHash: collection.EmptyHash}, nil
}

const collectionFinalizer = "kit.openshift.io/collection-protection"

func newCollectionTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	return scheme
}

func newCollectionClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newCollectionTestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.ResourceCollection{}).
		Build()
}

func reconcileCollection(t *testing.T, kubeClient client.Client, name string) ctrl.Result {
	t.Helper()

	result, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), nil, nil, &stubScriptLoader{}, "", "", nil).
		Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name},
		})
	require.NoError(t, err)
	return result
}

// TestCollectionDeletionBlockedByScriptCollectionRef verifies that a collection
// referenced by another collection's scriptCollections retains its finalizer and
// requeues instead of being deleted.
func TestCollectionDeletionBlockedByScriptCollectionRef(t *testing.T) {
	now := metav1.Now()
	target := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "common-scripts",
			DeletionTimestamp: &now,
			Finalizers:        []string{collectionFinalizer},
		},
	}
	referrer := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{Name: "mco-stubs"},
		Spec: v1alpha1.ResourceCollectionSpec{
			ScriptCollections: []string{"common-scripts"},
		},
	}

	kubeClient := newCollectionClient(t, target, referrer)
	result := reconcileCollection(t, kubeClient, "common-scripts")

	assert.Equal(t, 10*time.Second, result.RequeueAfter)

	var rc v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "common-scripts"}, &rc))
	assert.Contains(t, rc.Finalizers, collectionFinalizer)
}

// TestCollectionDeletionAllowedWhenReferrerDeleting verifies that a collection
// whose only scriptCollections referrer is itself being deleted is not
// considered referenced, so its finalizer is removed and deletion proceeds.
func TestCollectionDeletionAllowedWhenReferrerDeleting(t *testing.T) {
	now := metav1.Now()
	target := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "common-scripts",
			DeletionTimestamp: &now,
			Finalizers:        []string{collectionFinalizer},
		},
	}
	referrer := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "mco-stubs",
			DeletionTimestamp: &now,
			Finalizers:        []string{collectionFinalizer},
		},
		Spec: v1alpha1.ResourceCollectionSpec{
			ScriptCollections: []string{"common-scripts"},
		},
	}

	kubeClient := newCollectionClient(t, target, referrer)
	result := reconcileCollection(t, kubeClient, "common-scripts")

	assert.Zero(t, result.RequeueAfter)

	var rc v1alpha1.ResourceCollection
	err := kubeClient.Get(context.Background(), client.ObjectKey{Name: "common-scripts"}, &rc)
	assert.True(t, apierrors.IsNotFound(err), "object should be deleted when referrer is also deleting")
}

// TestCollectionDeletionAllowedWhenNoRef verifies that a finalized collection
// with no active scriptCollections referrers has its finalizer removed and is
// deleted.
func TestCollectionDeletionAllowedWhenNoRef(t *testing.T) {
	now := metav1.Now()
	target := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "common-scripts",
			DeletionTimestamp: &now,
			Finalizers:        []string{collectionFinalizer},
		},
	}
	other := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{Name: "mco-stubs"},
	}

	kubeClient := newCollectionClient(t, target, other)
	result := reconcileCollection(t, kubeClient, "common-scripts")

	assert.Zero(t, result.RequeueAfter)

	var rc v1alpha1.ResourceCollection
	err := kubeClient.Get(context.Background(), client.ObjectKey{Name: "common-scripts"}, &rc)
	assert.True(t, apierrors.IsNotFound(err), "object should be deleted after finalizer removal")
}

func TestCollectionSourceMissingSetsCondition(t *testing.T) {
	rc := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "nonexistent-source",
			Finalizers: []string{collectionFinalizer},
		},
		Spec: v1alpha1.ResourceCollectionSpec{
			Source: "nonexistent-source",
		},
	}
	plan := &v1alpha1.DeploymentPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "test-plan"},
		Spec: v1alpha1.DeploymentPlanSpec{
			Steps: []v1alpha1.DeploymentStep{
				{Name: "step-1", Collection: &v1alpha1.CollectionStep{Source: "nonexistent-source"}},
			},
		},
		Status: v1alpha1.DeploymentPlanStatus{
			Steps: []v1alpha1.DeploymentStepStatus{
				{Name: "step-1", Phase: v1alpha1.StepRequested},
			},
		},
	}
	kc := &v1alpha1.KitCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Status: v1alpha1.KitClusterStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}

	kubeClient := newCollectionClient(t, rc, plan, kc)
	_, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), nil, nil, &stubScriptLoader{}, t.TempDir(), "", nil).
		Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "nonexistent-source"},
		})
	require.Error(t, err)

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "nonexistent-source"}, &updated))

	contentCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionContentReady)
	require.NotNil(t, contentCond, "ContentReady should be set")
	assert.Equal(t, metav1.ConditionFalse, contentCond.Status)
	assert.Equal(t, "SourceMissing", contentCond.Reason)

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond, "Applied should also be set for plan-referenced collection")
	assert.Equal(t, metav1.ConditionFalse, appliedCond.Status)
	assert.Equal(t, "SourceMissing", appliedCond.Reason)
}

func TestCollectionAgentOnlyGetsReadyWithoutApply(t *testing.T) {
	rc := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "scripts-coll",
			Finalizers: []string{collectionFinalizer},
		},
		Spec: v1alpha1.ResourceCollectionSpec{
			Scripts: []v1alpha1.ScriptPackage{
				{Name: "my-pkg", Inline: "code"},
			},
		},
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Script: v1alpha1.AgentScriptRef{
				Collection: "scripts-coll",
				Package:    "my-pkg",
			},
		},
	}
	kc := &v1alpha1.KitCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Status: v1alpha1.KitClusterStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}

	kubeClient := newCollectionClient(t, rc, agent, kc)
	_, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), nil, nil, &stubScriptLoader{}, "", "", nil).
		Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "scripts-coll"},
		})
	require.NoError(t, err)

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "scripts-coll"}, &updated))

	contentCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionContentReady)
	require.NotNil(t, contentCond, "ContentReady should be set for agent-only collection")
	assert.Equal(t, metav1.ConditionTrue, contentCond.Status)
	assert.Equal(t, "Ready", contentCond.Reason)

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	assert.Nil(t, appliedCond, "Applied should not be set for agent-only collection")

	assert.NotEmpty(t, updated.Status.ContentHash)
}

func TestCollectionSourceMissingViaAgent(t *testing.T) {
	rc := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "missing-source",
			Finalizers: []string{collectionFinalizer},
		},
		Spec: v1alpha1.ResourceCollectionSpec{
			Source: "missing-source",
		},
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Script: v1alpha1.AgentScriptRef{
				Collection: "missing-source",
				Package:    "pkg",
			},
		},
	}
	kc := &v1alpha1.KitCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Status: v1alpha1.KitClusterStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}

	kubeClient := newCollectionClient(t, rc, agent, kc)
	_, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), nil, nil, &stubScriptLoader{}, t.TempDir(), "", nil).
		Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "missing-source"},
		})
	require.Error(t, err)

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "missing-source"}, &updated))

	contentCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionContentReady)
	require.NotNil(t, contentCond, "ContentReady should report SourceMissing for agent-only collections")
	assert.Equal(t, metav1.ConditionFalse, contentCond.Status)
	assert.Equal(t, "SourceMissing", contentCond.Reason)

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	assert.Nil(t, appliedCond, "Applied should not be set for agent-only collection")
}

func TestCollectionAgentOnlyRecoverFromSourceMissing(t *testing.T) {
	// Compute the actual combined hash that the reconciler will produce.
	combinedHash, err := collection.CombineContentHash(collection.ContentHashComponents{
		RenderHash:        collection.EmptyHash,
		ScriptHash:        collection.EmptyHash,
		StatusPatchesHash: collection.EmptyHash,
	})
	require.NoError(t, err)

	rc := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "scripts-coll",
			Finalizers: []string{collectionFinalizer},
		},
		Spec: v1alpha1.ResourceCollectionSpec{
			Scripts: []v1alpha1.ScriptPackage{
				{Name: "my-pkg", Inline: "code"},
			},
		},
		Status: v1alpha1.ResourceCollectionStatus{
			ContentHash: combinedHash,
			Conditions: []metav1.Condition{
				{
					Type:   v1alpha1.ConditionCollectionContentReady,
					Status: metav1.ConditionFalse,
					Reason: "SourceMissing",
				},
			},
		},
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Script: v1alpha1.AgentScriptRef{
				Collection: "scripts-coll",
				Package:    "my-pkg",
			},
		},
	}
	kc := &v1alpha1.KitCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Status: v1alpha1.KitClusterStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}

	kubeClient := newCollectionClient(t, rc, agent, kc)
	_, err = reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), nil, nil, &stubScriptLoader{}, "", "", nil).
		Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "scripts-coll"},
		})
	require.NoError(t, err)

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "scripts-coll"}, &updated))

	contentCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionContentReady)
	require.NotNil(t, contentCond, "ContentReady should be updated after recovery")
	assert.Equal(t, metav1.ConditionTrue, contentCond.Status)
	assert.Equal(t, "Ready", contentCond.Reason)
}

func TestCollectionAgentOnlyToPlanTriggersApply(t *testing.T) {
	combinedHash, err := collection.CombineContentHash(collection.ContentHashComponents{
		RenderHash:        collection.EmptyHash,
		ScriptHash:        collection.EmptyHash,
		StatusPatchesHash: collection.EmptyHash,
	})
	require.NoError(t, err)

	rc := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "scripts-coll",
			Finalizers: []string{collectionFinalizer},
		},
		Spec: v1alpha1.ResourceCollectionSpec{
			Scripts: []v1alpha1.ScriptPackage{
				{Name: "my-pkg", Inline: "code"},
			},
		},
		Status: v1alpha1.ResourceCollectionStatus{
			ContentHash: combinedHash,
			Conditions: []metav1.Condition{
				{
					Type:   v1alpha1.ConditionCollectionContentReady,
					Status: metav1.ConditionTrue,
					Reason: "Ready",
				},
			},
		},
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Script: v1alpha1.AgentScriptRef{
				Collection: "scripts-coll",
				Package:    "my-pkg",
			},
		},
	}
	plan := &v1alpha1.DeploymentPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "test-plan"},
		Spec: v1alpha1.DeploymentPlanSpec{
			Steps: []v1alpha1.DeploymentStep{
				{Name: "step-1", Collection: &v1alpha1.CollectionStep{Source: "scripts-coll"}},
			},
		},
		Status: v1alpha1.DeploymentPlanStatus{
			Steps: []v1alpha1.DeploymentStepStatus{
				{Name: "step-1", Phase: v1alpha1.StepRequested},
			},
		},
	}
	kc := &v1alpha1.KitCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Status: v1alpha1.KitClusterStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}

	kubeClient := newCollectionClient(t, rc, agent, plan, kc)
	_, err = reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), nil, nil, &stubScriptLoader{}, "", "", nil).
		Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "scripts-coll"},
		})
	require.NoError(t, err)

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "scripts-coll"}, &updated))

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond, "Applied should be set after plan requests the collection")
	assert.Equal(t, metav1.ConditionTrue, appliedCond.Status)
	assert.Equal(t, "Applied", appliedCond.Reason)
}
