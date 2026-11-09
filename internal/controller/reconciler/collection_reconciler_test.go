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
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
		Build()
}

func reconcileCollection(t *testing.T, kubeClient client.Client, name string) ctrl.Result {
	t.Helper()

	result, err := reconciler.NewCollectionReconciler(kubeClient, slog.New(slog.DiscardHandler), nil, nil, "", "", nil).
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
