package reconciler_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/workload/override"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestWorkloadOverrideReconcileNoMatch verifies that an override with no live
// matching Pods reports NoMatch and leaves unrelated Pods untouched.
func TestWorkloadOverrideReconcileNoMatch(t *testing.T) {
	wo := newWorkloadOverride("override", v1alpha1.WorkloadOverrideMatch{
		Labels: map[string]string{"app": "expected"},
	}, nil)
	pod := newPod("pod", "default", map[string]string{"app": "other"}, "old-image", true)

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(0), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "NoMatch")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileApplied verifies that an already-mutated Pod is
// counted as applied and is not deleted by remediation.
func TestWorkloadOverrideReconcileApplied(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "registry.example/new-image", true)

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(1), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionTrue, "Applied")
	assert.Contains(t, workloadOverrideCondition(t, status).Message, "1/1 matching pods use the override")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcilePendingApplicationDeletesControllerOwnedPod
// verifies that a Pod admitted before the override is deleted so its controller
// can recreate it and admission can apply the new specification.
func TestWorkloadOverrideReconcilePendingApplicationDeletesControllerOwnedPod(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "PendingApplication")
	assertPodNotFound(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileDeletesAllPendingTargetPods verifies that every
// matching controller-owned Pod requiring remediation is deleted, while an
// already applied Pod and an unrelated Pod remain unchanged.
func TestWorkloadOverrideReconcileDeletesAllPendingTargetPods(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	firstTarget := newPod("first-target", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)
	secondTarget := newPod("second-target", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)
	appliedPod := newPod("applied", "default", map[string]string{"app": "app"}, "registry.example/new-image", true)
	unrelatedPod := newPod("unrelated", "default", map[string]string{"app": "other"}, "registry.example/old-image", true)

	kubeClient := newWorkloadOverrideClient(t, wo, firstTarget, secondTarget, appliedPod, unrelatedPod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(3), status.MatchedPods)
	assert.Equal(t, int32(1), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "PendingApplication")
	assertPodNotFound(t, kubeClient, firstTarget)
	assertPodNotFound(t, kubeClient, secondTarget)
	assertPodUnchanged(t, kubeClient, appliedPod)
	assertPodUnchanged(t, kubeClient, unrelatedPod)
}

// TestWorkloadOverrideReconcileRecreationUnsupported verifies that a standalone
// Pod is not deleted when kit cannot recreate it from an owner template.
func TestWorkloadOverrideReconcileRecreationUnsupported(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "registry.example/old-image", false)

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "RecreationUnsupported")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileInvalid verifies that an override targeting a
// missing container is observable as Invalid and does not delete the Pod.
func TestWorkloadOverrideReconcileInvalid(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "missing", Image: "registry.example/new-image"},
	})
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "Invalid")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileFullySuperseded verifies that a broad override
// reports zero applied Pods when a more specific override wins every match.
func TestWorkloadOverrideReconcileFullySuperseded(t *testing.T) {
	broad := newWorkloadOverride("broad", v1alpha1.WorkloadOverrideMatch{
		Namespace: "default",
	}, []v1alpha1.ContainerOverride{{Name: "app", Image: "registry.example/broad"}})
	specific := newWorkloadOverride("specific", v1alpha1.WorkloadOverrideMatch{
		Namespace: "default",
		Labels:    map[string]string{"app": "app"},
	}, []v1alpha1.ContainerOverride{{Name: "app", Image: "registry.example/specific"}})
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "registry.example/specific", true)

	kubeClient := newWorkloadOverrideClient(t, broad, specific, pod)
	reconcileWorkloadOverride(t, kubeClient, broad.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, broad.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "SupersededOverride")
	assert.Contains(t, workloadOverrideCondition(t, status).Message, "specific")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcilePartiallySuperseded verifies that matched Pods
// include Pods won by a more specific override while applied Pods count only
// the Pods still controlled by this override.
func TestWorkloadOverrideReconcilePartiallySuperseded(t *testing.T) {
	broad := newWorkloadOverride("broad", v1alpha1.WorkloadOverrideMatch{
		Namespace: "default",
	}, []v1alpha1.ContainerOverride{{Name: "app", Image: "registry.example/broad"}})
	specific := newWorkloadOverride("specific", v1alpha1.WorkloadOverrideMatch{
		Namespace: "default",
		Labels:    map[string]string{"app": "specific"},
	}, []v1alpha1.ContainerOverride{{Name: "app", Image: "registry.example/specific"}})
	specificPod := newPod("specific", "default", map[string]string{"app": "specific"}, "registry.example/specific", true)
	broadPod := newPod("broad", "default", map[string]string{"app": "broad"}, "registry.example/broad", true)

	kubeClient := newWorkloadOverrideClient(t, broad, specific, specificPod, broadPod)
	reconcileWorkloadOverride(t, kubeClient, broad.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, broad.Name)
	assert.Equal(t, int32(2), status.MatchedPods)
	assert.Equal(t, int32(1), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "SupersededOverride")
	assert.Contains(t, workloadOverrideCondition(t, status).Message, "specific")
	assertPodUnchanged(t, kubeClient, specificPod)
	assertPodUnchanged(t, kubeClient, broadPod)
}

// TestWorkloadOverrideReconcilePartialSupersessionWithPendingPod verifies that
// an actionable pending selected Pod takes precedence over supersession and
// that the message still identifies the more specific winner.
func TestWorkloadOverrideReconcilePartialSupersessionWithPendingPod(t *testing.T) {
	broad := newWorkloadOverride("broad", v1alpha1.WorkloadOverrideMatch{
		Namespace: "default",
	}, []v1alpha1.ContainerOverride{{Name: "app", Image: "registry.example/broad"}})
	specific := newWorkloadOverride("specific", v1alpha1.WorkloadOverrideMatch{
		Namespace: "default",
		Labels:    map[string]string{"app": "specific"},
	}, []v1alpha1.ContainerOverride{{Name: "app", Image: "registry.example/specific"}})
	specificPod := newPod("specific", "default", map[string]string{"app": "specific"}, "registry.example/specific", true)
	appliedPod := newPod("applied", "default", map[string]string{"app": "applied"}, "registry.example/broad", true)
	pendingPod := newPod("pending", "default", map[string]string{"app": "pending"}, "registry.example/old-image", true)

	kubeClient := newWorkloadOverrideClient(t, broad, specific, specificPod, appliedPod, pendingPod)
	reconcileWorkloadOverride(t, kubeClient, broad.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, broad.Name)
	assert.Equal(t, int32(3), status.MatchedPods)
	assert.Equal(t, int32(1), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "PendingApplication")
	message := workloadOverrideCondition(t, status).Message
	assert.Contains(t, message, "1/2 selected pods have the override")
	assert.Contains(t, message, "1/3 matching pods superseded by specific")
	assertPodUnchanged(t, kubeClient, specificPod)
	assertPodUnchanged(t, kubeClient, appliedPod)
	assertPodNotFound(t, kubeClient, pendingPod)
}

// TestWorkloadOverrideReconcileNoOpApplied verifies that the canonical no-op
// Pod shape is recognized as applied without triggering recreation.
func TestWorkloadOverrideReconcileNoOpApplied(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), nil)
	wo.Spec.NoOp = true
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "", true)
	override.ApplyNoOp(pod)

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(1), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionTrue, "Applied")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileNoOpPending verifies that a real workload is
// recreated so admission can replace it with the canonical no-op shape.
func TestWorkloadOverrideReconcileNoOpPending(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), nil)
	wo.Spec.NoOp = true
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "PendingApplication")
	assertPodNotFound(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileExcludesTerminalPods verifies that succeeded
// and failed Pods do not affect live override counts or remediation.
func TestWorkloadOverrideReconcileExcludesTerminalPods(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	running := newPod("running", "default", map[string]string{"app": "app"}, "registry.example/new-image", true)
	succeeded := newPod("succeeded", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)
	succeeded.Status.Phase = corev1.PodSucceeded
	failed := newPod("failed", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)
	failed.Status.Phase = corev1.PodFailed

	kubeClient := newWorkloadOverrideClient(t, wo, running, succeeded, failed)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(1), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionTrue, "Applied")
	assertPodUnchanged(t, kubeClient, succeeded)
	assertPodUnchanged(t, kubeClient, failed)
}

// TestWorkloadOverrideReconcileNodeNameDoesNotDeleteScheduledPod verifies that
// an ordinary Pod scheduled after admission is not deleted in a loop when a
// nodeName override only starts matching after scheduling.
func TestWorkloadOverrideReconcileNodeNameDoesNotDeleteScheduledPod(t *testing.T) {
	wo := newWorkloadOverride("override", v1alpha1.WorkloadOverrideMatch{
		NodeName: "worker-0",
	}, []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	pod := newPodWithControllerKind(
		"pod", "default", map[string]string{"app": "app"}, "registry.example/old-image", "ReplicaSet",
	)
	pod.Spec.NodeName = "worker-0"

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "RecreationUnsupported")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileNodeNameDeletesDaemonSetPod verifies that a
// controller-owned Pod created with an explicit node assignment can be safely
// recreated for a nodeName override.
func TestWorkloadOverrideReconcileNodeNameDeletesDaemonSetPod(t *testing.T) {
	wo := newWorkloadOverride("override", v1alpha1.WorkloadOverrideMatch{
		NodeName: "worker-0",
	}, []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	pod := newPodWithControllerKind(
		"pod", "default", map[string]string{"app": "app"}, "registry.example/old-image", "DaemonSet",
	)
	pod.Spec.NodeName = "worker-0"

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "PendingApplication")
	assertPodNotFound(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileSkipsDeleteForDeletingPod verifies that a Pod
// already being deleted is left to finish termination instead of receiving
// another delete request.
func TestWorkloadOverrideReconcileSkipsDeleteForDeletingPod(t *testing.T) {
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)
	deletionTime := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	pod.DeletionTimestamp = &deletionTime
	pod.Finalizers = []string{"kit.example/test"}

	kubeClient := newWorkloadOverrideClient(t, wo, pod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "PendingApplication")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileMissingOverrideIsNoOp verifies that reconcile
// requests for an already deleted WorkloadOverride return success.
func TestWorkloadOverrideReconcileMissingOverrideIsNoOp(t *testing.T) {
	kubeClient := newWorkloadOverrideClient(t)

	result, err := reconciler.NewWorkloadOverrideReconciler(kubeClient, slog.New(slog.DiscardHandler)).
		Reconcile(context.Background(), workloadOverrideRequest("missing"))
	assert.Equal(t, ctrl.Result{}, result)
	require.NoError(t, err)
}

// TestWorkloadOverrideReconcileListsOnlyConfiguredNamespace verifies that a
// namespace-scoped override counts and remediates only Pods in that namespace.
func TestWorkloadOverrideReconcileListsOnlyConfiguredNamespace(t *testing.T) {
	wo := newWorkloadOverride("override", v1alpha1.WorkloadOverrideMatch{
		Namespace: "namespace-a",
		Labels:    map[string]string{"app": "app"},
	}, []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	matchingPod := newPod("matching", "namespace-a", map[string]string{"app": "app"}, "registry.example/old-image", true)
	otherNamespacePod := newPod("other", "namespace-b", map[string]string{"app": "app"}, "registry.example/old-image", true)

	kubeClient := newWorkloadOverrideClient(t, wo, matchingPod, otherNamespacePod)
	reconcileWorkloadOverride(t, kubeClient, wo.Name)

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assert.Equal(t, int32(1), status.MatchedPods)
	assert.Equal(t, int32(0), status.AppliedPods)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "PendingApplication")
	assertPodNotFound(t, kubeClient, matchingPod)
	assertPodUnchanged(t, kubeClient, otherNamespacePod)
}

// TestWorkloadOverrideReconcileDeleteError verifies that a failed Pod deletion
// is returned to the workqueue and surfaced through the Failed condition.
func TestWorkloadOverrideReconcileDeleteError(t *testing.T) {
	ctx := context.Background()
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	pod := newPod("pod", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)
	deleteErr := apierrors.NewForbidden(
		corev1.Resource("pods"), pod.Name, assert.AnError,
	)

	kubeClient := fake.NewClientBuilder().
		WithScheme(newWorkloadOverrideTestScheme(t)).
		WithStatusSubresource(&v1alpha1.WorkloadOverride{}).
		WithObjects(wo, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return deleteErr
			},
		}).
		Build()

	result, err := reconciler.NewWorkloadOverrideReconciler(kubeClient, slog.New(slog.DiscardHandler)).
		Reconcile(ctx, workloadOverrideRequest(wo.Name))
	assert.Equal(t, ctrl.Result{}, result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deleting pod default/pod")

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "Failed")
	assert.Contains(t, workloadOverrideCondition(t, status).Message, "pods \"pod\" is forbidden")
	assertPodUnchanged(t, kubeClient, pod)
}

// TestWorkloadOverrideReconcileMultipleDeleteErrors verifies that all Pod
// deletion failures are retained in the returned error and status message.
func TestWorkloadOverrideReconcileMultipleDeleteErrors(t *testing.T) {
	ctx := context.Background()
	wo := newWorkloadOverride("override", matchByApp("app"), []v1alpha1.ContainerOverride{
		{Name: "app", Image: "registry.example/new-image"},
	})
	first := newPod("first", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)
	second := newPod("second", "default", map[string]string{"app": "app"}, "registry.example/old-image", true)

	kubeClient := fake.NewClientBuilder().
		WithScheme(newWorkloadOverrideTestScheme(t)).
		WithStatusSubresource(&v1alpha1.WorkloadOverride{}).
		WithObjects(wo, first, second).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(corev1.Resource("pods"), obj.GetName(), assert.AnError)
			},
		}).
		Build()

	_, err := reconciler.NewWorkloadOverrideReconciler(kubeClient, slog.New(slog.DiscardHandler)).
		Reconcile(ctx, workloadOverrideRequest(wo.Name))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deleting pod default/first")
	assert.Contains(t, err.Error(), "deleting pod default/second")

	status := getWorkloadOverrideStatus(t, kubeClient, wo.Name)
	assertWorkloadOverrideCondition(t, status, metav1.ConditionFalse, "Failed")
	conditionMessage := workloadOverrideCondition(t, status).Message
	assert.Contains(t, conditionMessage, "deleting pod default/first")
	assert.Contains(t, conditionMessage, "deleting pod default/second")
}

func newWorkloadOverrideTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	return scheme
}

func newWorkloadOverrideClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newWorkloadOverrideTestScheme(t)).
		WithStatusSubresource(&v1alpha1.WorkloadOverride{}).
		WithObjects(objects...).
		Build()
}

func newWorkloadOverride(
	name string,
	match v1alpha1.WorkloadOverrideMatch,
	containers []v1alpha1.ContainerOverride,
) *v1alpha1.WorkloadOverride {
	return &v1alpha1.WorkloadOverride{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.WorkloadOverrideSpec{
			Match:      match,
			Containers: containers,
		},
	}
}

func matchByApp(value string) v1alpha1.WorkloadOverrideMatch {
	return v1alpha1.WorkloadOverrideMatch{Labels: map[string]string{"app": value}}
}

func newPod(name, namespace string, labels map[string]string, image string, controlled bool) *corev1.Pod {
	ownerKind := ""
	if controlled {
		ownerKind = "DaemonSet"
	}
	return newPodWithControllerKind(name, namespace, labels, image, ownerKind)
}

func newPodWithControllerKind(
	name, namespace string,
	labels map[string]string,
	image string,
	ownerKind string,
) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: image}},
		},
	}
	if ownerKind != "" {
		controller := true
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1",
			Kind:       ownerKind,
			Name:       "owner",
			UID:        "00000000-0000-0000-0000-000000000001",
			Controller: &controller,
		}}
	}
	return pod
}

func reconcileWorkloadOverride(t *testing.T, kubeClient client.WithWatch, name string) {
	t.Helper()

	_, err := reconciler.NewWorkloadOverrideReconciler(kubeClient, slog.New(slog.DiscardHandler)).
		Reconcile(context.Background(), workloadOverrideRequest(name))
	require.NoError(t, err)
}

func workloadOverrideRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}

func getWorkloadOverrideStatus(t *testing.T, kubeClient client.Client, name string) v1alpha1.WorkloadOverrideStatus {
	t.Helper()

	var wo v1alpha1.WorkloadOverride
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{Name: name}, &wo))
	return wo.Status
}

func assertWorkloadOverrideCondition(
	t *testing.T,
	status v1alpha1.WorkloadOverrideStatus,
	conditionStatus metav1.ConditionStatus,
	reason string,
) {
	t.Helper()

	condition := meta.FindStatusCondition(status.Conditions, v1alpha1.ConditionOverrideApplied)
	require.NotNil(t, condition)
	assert.Equal(t, conditionStatus, condition.Status)
	assert.Equal(t, reason, condition.Reason)
}

func workloadOverrideCondition(
	t *testing.T,
	status v1alpha1.WorkloadOverrideStatus,
) *metav1.Condition {
	t.Helper()

	condition := meta.FindStatusCondition(status.Conditions, v1alpha1.ConditionOverrideApplied)
	require.NotNil(t, condition)
	return condition
}

func assertPodUnchanged(t *testing.T, kubeClient client.Client, pod *corev1.Pod) {
	t.Helper()

	var got corev1.Pod
	require.NoError(t, kubeClient.Get(
		context.Background(),
		types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
		&got,
	))
	assert.Equal(t, pod.Labels, got.Labels, "pod labels should remain unchanged")
	assert.Equal(t, pod.Annotations, got.Annotations, "pod annotations should remain unchanged")
	assert.Equal(t, pod.OwnerReferences, got.OwnerReferences, "pod owners should remain unchanged")
	assert.Equal(t, pod.Finalizers, got.Finalizers, "pod finalizers should remain unchanged")
	if pod.DeletionTimestamp == nil || got.DeletionTimestamp == nil {
		assert.Equal(t, pod.DeletionTimestamp, got.DeletionTimestamp, "pod deletion timestamp should remain unchanged")
	} else {
		assert.True(t, pod.DeletionTimestamp.Equal(got.DeletionTimestamp), "pod deletion timestamp should remain unchanged")
	}
	assert.Equal(t, pod.Spec, got.Spec, "pod spec should remain unchanged")
	assert.Equal(t, pod.Status, got.Status, "pod status should remain unchanged")
}

func assertPodNotFound(t *testing.T, kubeClient client.Client, pod *corev1.Pod) {
	t.Helper()

	var got corev1.Pod
	err := kubeClient.Get(
		context.Background(),
		types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
		&got,
	)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
}
