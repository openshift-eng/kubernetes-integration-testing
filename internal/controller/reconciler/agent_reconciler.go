package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	agentctl "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent"
	agentrbac "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent/rbac"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type agentReconciler struct {
	client       client.Client
	materializer *agentctl.Materializer
	log          *slog.Logger
}

const apiTypePendingRequeueAfter = 5 * time.Second

func NewAgentReconciler(c client.Client, mat *agentctl.Materializer, log *slog.Logger) *agentReconciler {
	return &agentReconciler{client: c, materializer: mat, log: log}
}

func (r *agentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ag v1alpha1.Agent
	if err := r.client.Get(ctx, req.NamespacedName, &ag); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var ns corev1.Namespace
	if err := r.client.Get(ctx, client.ObjectKey{Name: ag.Spec.Target.Namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return r.patchStatus(ctx, &ag, "", metav1.Condition{
				Type:               v1alpha1.ConditionAgentReady,
				Status:             metav1.ConditionFalse,
				Reason:             v1alpha1.ReasonAgentNamespacePending,
				Message:            "target namespace does not exist yet",
				ObservedGeneration: ag.Generation,
			})
		}
		return ctrl.Result{}, err
	}

	result, err := r.materializer.Materialize(ctx, &ag)
	if err != nil {
		var pending *agentrbac.APITypePendingError
		if errors.As(err, &pending) {
			if _, patchErr := r.patchStatus(ctx, &ag, "", metav1.Condition{
				Type:               v1alpha1.ConditionAgentReady,
				Status:             metav1.ConditionFalse,
				Reason:             v1alpha1.ReasonAgentAPITypePending,
				Message:            err.Error(),
				ObservedGeneration: ag.Generation,
			}); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{RequeueAfter: apiTypePendingRequeueAfter}, nil
		}

		var invalid *agentrbac.InvalidCapabilitiesError
		if errors.As(err, &invalid) {
			return r.patchStatus(ctx, &ag, "", metav1.Condition{
				Type:               v1alpha1.ConditionAgentReady,
				Status:             metav1.ConditionFalse,
				Reason:             v1alpha1.ReasonAgentInvalidCapabilities,
				Message:            err.Error(),
				ObservedGeneration: ag.Generation,
			})
		}

		reason := v1alpha1.ReasonAgentCollectionError
		_, patchErr := r.patchStatus(ctx, &ag, "", metav1.Condition{
			Type:               v1alpha1.ConditionAgentReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            err.Error(),
			ObservedGeneration: ag.Generation,
		})
		if patchErr != nil {
			r.log.Error("patching agent status after materialization error", "agent", ag.Name, "error", patchErr)
		}
		return ctrl.Result{}, err
	}

	return r.patchStatus(ctx, &ag, result.BundleDigest, metav1.Condition{
		Type:               v1alpha1.ConditionAgentReady,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonAgentReady,
		Message:            "agent materialized successfully",
		ObservedGeneration: ag.Generation,
	})
}

func (r *agentReconciler) patchStatus(ctx context.Context, ag *v1alpha1.Agent, digest string, cond metav1.Condition) (ctrl.Result, error) {
	base := ag.DeepCopy()
	ag.Status.ObservedGeneration = ag.Generation
	ag.Status.BundleDigest = digest
	meta.SetStatusCondition(&ag.Status.Conditions, cond)
	if err := r.client.Status().Patch(ctx, ag, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *agentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("agent").
		For(&v1alpha1.Agent{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&v1alpha1.WorkloadOverride{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.namespaceToAgents)).
		Watches(&v1alpha1.ResourceCollection{}, handler.EnqueueRequestsFromMapFunc(r.collectionToAgents)).
		Complete(r)
}

func (r *agentReconciler) namespaceToAgents(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents v1alpha1.AgentList
	if err := r.client.List(ctx, &agents); err != nil {
		r.log.Error("listing agents for namespace watch", "error", err)
		return nil
	}
	var requests []reconcile.Request
	for _, ag := range agents.Items {
		if ag.Spec.Target.Namespace == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ag)})
		}
	}
	return requests
}

func (r *agentReconciler) collectionToAgents(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents v1alpha1.AgentList
	if err := r.client.List(ctx, &agents); err != nil {
		r.log.Error("listing agents for collection watch", "error", err)
		return nil
	}
	var requests []reconcile.Request
	for _, ag := range agents.Items {
		if ag.Spec.Script.Collection == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ag)})
		}
	}
	return requests
}
