// TODO: These tests only cover scriptCollections finalizer protection. Missing
// coverage for the rest of the collection reconciler: rendering (source, inline,
// values, funcmap), apply (SSA, namespace ordering), status patching (CEL
// evaluation, target resolution), content-hash skip, KitCluster readiness
// gating, DeploymentPlan request filtering, filesystem watcher, and auto-creation
// of builtin/on-disk collections.
package reconciler_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"

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

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond, "Ready should mirror the content failure")
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, "SourceMissing", readyCond.Reason)
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
	require.NotNil(t, appliedCond, "Applied should be set even without a plan")
	assert.Equal(t, metav1.ConditionTrue, appliedCond.Status)
	assert.Equal(t, "NothingToApply", appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond, "Ready should be set for agent-only collection")
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	assert.Equal(t, "Ready", readyCond.Reason)

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
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status, "nothing requested an apply, so Applied must not look degraded")
	assert.Equal(t, v1alpha1.ReasonCollectionAppliedNotRequested, appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond, "Ready should report the content failure")
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, "SourceMissing", readyCond.Reason)
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

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond, "Applied should be updated after recovery")
	assert.Equal(t, metav1.ConditionTrue, appliedCond.Status)
	assert.Equal(t, "NothingToApply", appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond, "Ready should be updated after recovery")
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	assert.Equal(t, "Ready", readyCond.Reason)
}

// fooTemplate renders a cluster-scoped example.com/v1 Foo, a kind the
// fake discovery in this package serves, so collection applies can be
// recorded without a real API server.
const fooTemplate = `apiVersion: example.com/v1
kind: Foo
metadata:
  name: demo
spec:
  value: %s
`

func readyKitCluster(name string) *v1alpha1.KitCluster {
	return &v1alpha1.KitCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1alpha1.KitClusterStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}
}

// inlineContentHash computes the content hash the reconciler produces for
// inline templates with no scripts and no status patches.
func inlineContentHash(t *testing.T, contents ...string) string {
	t.Helper()
	coll, err := collection.LoadInline("test", contents, nil)
	require.NoError(t, err)
	result, err := coll.Render(collection.RenderContext{})
	require.NoError(t, err)
	combined, err := collection.CombineContentHash(collection.ContentHashComponents{
		RenderHash:        result.Hash,
		ScriptHash:        collection.EmptyHash,
		StatusPatchesHash: collection.EmptyHash,
	})
	require.NoError(t, err)
	return combined
}

func templateCollection(name, value string) *v1alpha1.ResourceCollection {
	return &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Finalizers: []string{collectionFinalizer},
		},
		Spec: v1alpha1.ResourceCollectionSpec{
			Inline: []v1alpha1.InlineTemplate{{Content: fmt.Sprintf(fooTemplate, value)}},
		},
	}
}

func planWithStepPhase(name, source string, phase v1alpha1.DeploymentStepPhase) *v1alpha1.DeploymentPlan {
	return &v1alpha1.DeploymentPlan{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.DeploymentPlanSpec{
			Steps: []v1alpha1.DeploymentStep{
				{Name: "step-1", Collection: &v1alpha1.CollectionStep{Source: source}},
			},
		},
		Status: v1alpha1.DeploymentPlanStatus{
			Steps: []v1alpha1.DeploymentStepStatus{
				{Name: "step-1", Phase: phase},
			},
		},
	}
}

func reconcileCollectionWith(t *testing.T, kubeClient client.Client, clients *kube.Clients, name string) ctrl.Result {
	t.Helper()
	result, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), clients, nil, &stubScriptLoader{}, "", "", nil).
		Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name},
		})
	require.NoError(t, err)
	return result
}

// TestCollectionUnreferencedNotRendered verifies the consumption filter:
// a collection nothing references is not rendered (rendering can create
// certificates and secrets) and only gets Ready=Unknown/NotReferenced.
func TestCollectionUnreferencedNotRendered(t *testing.T) {
	rc := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "orphan-scripts",
			Finalizers: []string{collectionFinalizer},
		},
		Spec: v1alpha1.ResourceCollectionSpec{
			Scripts: []v1alpha1.ScriptPackage{
				{Name: "my-pkg", Inline: "code"},
			},
		},
	}

	kubeClient := newCollectionClient(t, rc, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, nil, "orphan-scripts")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "orphan-scripts"}, &updated))

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond, "Ready should be stamped as Unknown/NotReferenced")
	assert.Equal(t, metav1.ConditionUnknown, readyCond.Status)
	assert.Equal(t, "NotReferenced", readyCond.Reason)

	assert.Len(t, updated.Status.Conditions, 1, "nothing else should be evaluated")
	assert.Empty(t, updated.Status.ContentHash)
}

// TestCollectionTemplatesNotRequested verifies that a template collection
// consumed but not requested by any plan reports Applied=Unknown with
// AppliedNotRequested and Ready=Unknown.
func TestCollectionTemplatesNotRequested(t *testing.T) {
	rc := templateCollection("template-coll", "v1")
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Script: v1alpha1.AgentScriptRef{Collection: "template-coll", Package: "pkg"},
		},
	}

	clients, dyn := recordingClients()
	kubeClient := newCollectionClient(t, rc, agent, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	contentCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionContentReady)
	require.NotNil(t, contentCond)
	assert.Equal(t, metav1.ConditionTrue, contentCond.Status)

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status)
	assert.Equal(t, v1alpha1.ReasonCollectionAppliedNotRequested, appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status, "own health must not depend on consumers")
	assert.Equal(t, "Ready", readyCond.Reason)

	assert.Empty(t, dyn.appliedNames, "unrequested collection must not apply")
}

// TestCollectionTemplatesPlanPending verifies that a template collection
// referenced only by an unreached plan step reports Applied=Unknown with
// AppliedPlanPending instead of AppliedNotRequested, and that nothing is
// applied yet (sequencing).
func TestCollectionTemplatesPlanPending(t *testing.T) {
	rc := templateCollection("template-coll", "v1")
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepPending)

	clients, dyn := recordingClients()
	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status)
	assert.Equal(t, v1alpha1.ReasonCollectionAppliedPlanPending, appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status, "own health must not depend on plan progress")
	assert.Equal(t, "Ready", readyCond.Reason)

	assert.Empty(t, dyn.appliedNames, "pending step must not trigger an apply")
}

// TestCollectionNotReferencedClearsStaleWaitState verifies that when the
// last consumer disappears (e.g. a plan is deleted), Applied wait states
// are rewritten: their messages name consumers that no longer exist. The
// collection's own health stays as it was.
func TestCollectionNotReferencedClearsStaleWaitState(t *testing.T) {
	rc := templateCollection("template-coll", "v1")
	rc.Status = v1alpha1.ResourceCollectionStatus{
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionCollectionContentReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			{Type: v1alpha1.ConditionCollectionApplied, Status: metav1.ConditionUnknown, Reason: v1alpha1.ReasonCollectionAppliedPlanPending},
			{Type: v1alpha1.ConditionCollectionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}

	kubeClient := newCollectionClient(t, rc, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, nil, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond, "Applied should be rewritten")
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status)
	assert.Equal(t, v1alpha1.ReasonCollectionNotReferenced, appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond, "Ready should be kept")
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	assert.Equal(t, "Ready", readyCond.Reason)
}

// TestCollectionNotReferencedLeavesAppliedStatus verifies that losing the
// last consumer does not rewrite the status of an applied collection:
// the last apply still describes the cluster.
func TestCollectionNotReferencedLeavesAppliedStatus(t *testing.T) {
	rc := templateCollection("template-coll", "v1")
	rc.Status = v1alpha1.ResourceCollectionStatus{
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionCollectionContentReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			{Type: v1alpha1.ConditionCollectionApplied, Status: metav1.ConditionTrue, Reason: "Applied"},
			{Type: v1alpha1.ConditionCollectionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}

	kubeClient := newCollectionClient(t, rc, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, nil, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionTrue, appliedCond.Status)
	assert.Equal(t, "Applied", appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	assert.Equal(t, "Ready", readyCond.Reason)
}

// TestCollectionPlanRequestedAppliesTemplates verifies the happy path:
// a plan step in Requested phase applies the collection's templates and
// reports Applied=True and Ready=True.
func TestCollectionPlanRequestedAppliesTemplates(t *testing.T) {
	rc := templateCollection("template-coll", "v1")
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepRequested)

	clients, dyn := recordingClients()
	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond, "Applied should be set after the plan requests the collection")
	assert.Equal(t, metav1.ConditionTrue, appliedCond.Status)
	assert.Equal(t, "Applied", appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	assert.Equal(t, "Ready", readyCond.Reason)

	assert.Equal(t, []string{"demo"}, dyn.appliedNames)
}

// TestCollectionApplyFailureKeepsReady verifies that a failing apply is
// reported only through Applied: applies run on plan request, so the
// collection's own health (Ready) must not depend on them.
func TestCollectionApplyFailureKeepsReady(t *testing.T) {
	rc := templateCollection("template-coll", "v1")
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepRequested)

	clients, dyn := recordingClients()
	dyn.applyErr = errors.New("boom")
	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))

	_, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), clients, nil, &stubScriptLoader{}, "", "", nil).
		Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "template-coll"}})
	require.Error(t, err, "the apply error is returned so the reconcile retries")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionFalse, appliedCond.Status)
	assert.Equal(t, "ApplyFailed", appliedCond.Reason)
	assert.Contains(t, appliedCond.Message, "boom")

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status, "own health must not depend on the apply")
	assert.Equal(t, "Ready", readyCond.Reason)

	contentCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionContentReady)
	require.NotNil(t, contentCond)
	assert.Equal(t, metav1.ConditionTrue, contentCond.Status)
}

// brokenTemplateCollection returns a template collection whose content
// fails to render.
func brokenTemplateCollection(name string) *v1alpha1.ResourceCollection {
	rc := templateCollection(name, "v1")
	rc.Spec.Inline = []v1alpha1.InlineTemplate{{Content: "kind: Foo\nname: {{ .Broken"}}
	return rc
}

// reconcileCollectionExpectError reconciles a collection whose reconcile
// is expected to fail and returns its updated status.
func reconcileCollectionExpectError(t *testing.T, kubeClient client.Client, name string) v1alpha1.ResourceCollection {
	t.Helper()
	clients, _ := recordingClients()
	_, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), clients, nil, &stubScriptLoader{}, "", "", nil).
		Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.Error(t, err)
	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: name}, &updated))
	return updated
}

// TestCollectionContentFailureAfterApplyIsOutdated verifies that when the
// content of an applied collection stops rendering and no plan requests
// it, Applied reports the old content as outdated instead of degraded:
// nothing was attempted and the old content is still live.
func TestCollectionContentFailureAfterApplyIsOutdated(t *testing.T) {
	oldHash := inlineContentHash(t, fmt.Sprintf(fooTemplate, "old"))
	rc := brokenTemplateCollection("template-coll")
	rc.Status = v1alpha1.ResourceCollectionStatus{
		ContentHash: oldHash,
		AppliedHash: oldHash,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionCollectionContentReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			{Type: v1alpha1.ConditionCollectionApplied, Status: metav1.ConditionTrue, Reason: "Applied"},
			{Type: v1alpha1.ConditionCollectionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepReady)

	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))
	updated := reconcileCollectionExpectError(t, kubeClient, "template-coll")

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status)
	assert.Equal(t, v1alpha1.ReasonCollectionAppliedOutdated, appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status, "broken content is the collection's own health")
	assert.Equal(t, "RenderFailed", readyCond.Reason)

	assert.Equal(t, oldHash, updated.Status.ContentHash, "a render failure leaves ContentHash untouched")
	assert.Equal(t, oldHash, updated.Status.AppliedHash, "AppliedHash keeps describing the live content")
}

// TestCollectionContentFailureNeverAppliedKeepsWaitState verifies that a
// never-applied collection with broken content keeps its Applied wait
// state: nothing was attempted, so Applied must not look degraded.
func TestCollectionContentFailureNeverAppliedKeepsWaitState(t *testing.T) {
	rc := brokenTemplateCollection("template-coll")
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepPending)

	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))
	updated := reconcileCollectionExpectError(t, kubeClient, "template-coll")

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status)
	assert.Equal(t, v1alpha1.ReasonCollectionAppliedPlanPending, appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, "RenderFailed", readyCond.Reason)
}

// TestCollectionApplyFailureKeepsAppliedMemory verifies that a failed
// apply does not erase the record of an earlier successful one: the old
// content is still live, so once the request goes away (here, the plan is
// reset and its step is Pending again) the collection is reported as
// outdated, not as never applied.
func TestCollectionApplyFailureKeepsAppliedMemory(t *testing.T) {
	oldHash := inlineContentHash(t, fmt.Sprintf(fooTemplate, "old"))
	rc := templateCollection("template-coll", "new")
	rc.Status = v1alpha1.ResourceCollectionStatus{
		ContentHash: oldHash,
		AppliedHash: oldHash,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionCollectionContentReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			{Type: v1alpha1.ConditionCollectionApplied, Status: metav1.ConditionTrue, Reason: "Applied"},
			{Type: v1alpha1.ConditionCollectionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepRequested)

	clients, dyn := recordingClients()
	dyn.applyErr = errors.New("boom")
	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))

	_, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), clients, nil, &stubScriptLoader{}, "", "", nil).
		Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "template-coll"}})
	require.Error(t, err)

	var afterFailure v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &afterFailure))
	appliedCond := meta.FindStatusCondition(afterFailure.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionFalse, appliedCond.Status, "a requested apply that fails is reported")
	assert.Equal(t, oldHash, afterFailure.Status.AppliedHash, "the failed apply must not erase the live content record")

	// The plan is reset: the step is no longer requested.
	var current v1alpha1.DeploymentPlan
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "test-plan"}, &current))
	current.Status.Steps[0].Phase = v1alpha1.StepPending
	require.NoError(t, kubeClient.Update(context.Background(), &current))

	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))
	appliedCond = meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status)
	assert.Equal(t, v1alpha1.ReasonCollectionAppliedOutdated, appliedCond.Reason,
		"the old content is still live, so the collection is outdated, not pending")
	assert.Equal(t, oldHash, updated.Status.AppliedHash)
}

// unreferencedAppliedCollection returns a collection that was applied at
// generation 1 and that nothing references anymore.
func unreferencedAppliedCollection(generation int64) *v1alpha1.ResourceCollection {
	rc := templateCollection("template-coll", "v1")
	rc.Generation = generation
	rc.Status = v1alpha1.ResourceCollectionStatus{
		ContentHash: "applied-hash",
		AppliedHash: "applied-hash",
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionCollectionContentReady, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 1},
			{Type: v1alpha1.ConditionCollectionApplied, Status: metav1.ConditionTrue, Reason: "Applied", ObservedGeneration: 1},
			{Type: v1alpha1.ConditionCollectionReady, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 1},
		},
	}
	return rc
}

// TestCollectionNotReferencedEditIsOutdated verifies that an applied
// collection edited after its last consumer went away is reported as
// outdated, even though it is not rendered: the spec generation moved
// past the one at which the apply was last verified.
func TestCollectionNotReferencedEditIsOutdated(t *testing.T) {
	kubeClient := newCollectionClient(t, unreferencedAppliedCollection(2), readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, nil, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status)
	assert.Equal(t, v1alpha1.ReasonCollectionAppliedOutdated, appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status, "the last health verdict is kept")
	assert.Equal(t, "applied-hash", updated.Status.AppliedHash)
}

// TestCollectionNotReferencedUnchangedStaysApplied verifies that an
// applied collection whose spec did not change after its last consumer
// went away keeps Applied=True.
func TestCollectionNotReferencedUnchangedStaysApplied(t *testing.T) {
	kubeClient := newCollectionClient(t, unreferencedAppliedCollection(1), readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, nil, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionTrue, appliedCond.Status)
	assert.Equal(t, "Applied", appliedCond.Reason)
}

// TestCollectionCompletedPlanOutdatedOnEdit verifies that editing a
// collection after its plan step completed does not reapply: the drift is
// reflected as Applied=Unknown/AppliedOutdated while AppliedHash keeps
// describing the live content, and the state is stable across reconciles.
func TestCollectionCompletedPlanOutdatedOnEdit(t *testing.T) {
	oldHash := inlineContentHash(t, fmt.Sprintf(fooTemplate, "old"))
	newHash := inlineContentHash(t, fmt.Sprintf(fooTemplate, "new"))
	rc := templateCollection("template-coll", "new")
	rc.Status = v1alpha1.ResourceCollectionStatus{
		ContentHash: oldHash,
		AppliedHash: oldHash,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionCollectionContentReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			{Type: v1alpha1.ConditionCollectionApplied, Status: metav1.ConditionTrue, Reason: "Applied"},
			{Type: v1alpha1.ConditionCollectionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepReady)

	clients, dyn := recordingClients()
	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	assert.Empty(t, dyn.appliedNames, "no reapply after the plan completed")
	assert.Equal(t, newHash, updated.Status.ContentHash, "ContentHash describes the rendered content")
	assert.Equal(t, oldHash, updated.Status.AppliedHash, "AppliedHash keeps describing the live content")

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond, "the drift must be reflected")
	assert.Equal(t, metav1.ConditionUnknown, appliedCond.Status)
	assert.Equal(t, v1alpha1.ReasonCollectionAppliedOutdated, appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status, "own health must not depend on the drift")
	assert.Equal(t, "Ready", readyCond.Reason)

	// Second reconcile: the drift is unchanged, the status is stable.
	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var afterSecond v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &afterSecond))
	assert.Equal(t, updated.Status.Conditions, afterSecond.Status.Conditions)
	assert.Equal(t, oldHash, afterSecond.Status.AppliedHash)
	assert.Empty(t, dyn.appliedNames, "still no reapply while frozen")
}

// TestCollectionOutdatedReappliesWhenRequested verifies the end of the
// freeze cycle: once a plan step requests an outdated collection, the
// drifted content is applied and the status recovers to Applied=True with
// the new hash.
func TestCollectionOutdatedReappliesWhenRequested(t *testing.T) {
	oldHash := inlineContentHash(t, fmt.Sprintf(fooTemplate, "old"))
	newHash := inlineContentHash(t, fmt.Sprintf(fooTemplate, "new"))
	rc := templateCollection("template-coll", "new")
	rc.Status = v1alpha1.ResourceCollectionStatus{
		ContentHash: newHash,
		AppliedHash: oldHash,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionCollectionContentReady, Status: metav1.ConditionTrue, Reason: "Ready"},
			{Type: v1alpha1.ConditionCollectionApplied, Status: metav1.ConditionUnknown, Reason: v1alpha1.ReasonCollectionAppliedOutdated},
			{Type: v1alpha1.ConditionCollectionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepRequested)

	clients, dyn := recordingClients()
	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var updated v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &updated))

	assert.Equal(t, []string{"demo"}, dyn.appliedNames, "a requesting plan reapplies the drifted content")
	assert.Equal(t, newHash, updated.Status.ContentHash)
	assert.Equal(t, newHash, updated.Status.AppliedHash, "a successful apply records the new live content")

	appliedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	require.NotNil(t, appliedCond)
	assert.Equal(t, metav1.ConditionTrue, appliedCond.Status)
	assert.Equal(t, "Applied", appliedCond.Reason)

	readyCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionCollectionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	assert.Equal(t, "Ready", readyCond.Reason)
}

// TestCollectionSteadyStateSkipsStatusWrite verifies that a second
// reconcile of an unchanged, applied collection writes nothing new: the
// conditions and hash already describe the world.
func TestCollectionSteadyStateSkipsStatusWrite(t *testing.T) {
	rc := templateCollection("template-coll", "v1")
	plan := planWithStepPhase("test-plan", "template-coll", v1alpha1.StepRequested)

	clients, dyn := recordingClients()
	kubeClient := newCollectionClient(t, rc, plan, readyKitCluster("test-cluster"))
	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var afterFirst v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &afterFirst))
	require.Equal(t, []string{"demo"}, dyn.appliedNames)

	// Second reconcile: content and conditions unchanged, hash already
	// applied — no further applies, no condition churn.
	reconcileCollectionWith(t, kubeClient, clients, "template-coll")

	var afterSecond v1alpha1.ResourceCollection
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "template-coll"}, &afterSecond))

	assert.Equal(t, afterFirst.Status.ContentHash, afterSecond.Status.ContentHash)
	assert.Equal(t, afterFirst.Status.Conditions, afterSecond.Status.Conditions)
	assert.Equal(t, []string{"demo"}, dyn.appliedNames, "unchanged content must not be reapplied")
}
