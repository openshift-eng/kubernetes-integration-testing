package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/dynutil"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

type deploymentPlanReconciler struct {
	client    client.Client
	dynClient dynamic.Interface
	discovery discovery.DiscoveryInterface
	release   *release.ReleaseManifests
	log       *slog.Logger
}

func (r *deploymentPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var plan v1alpha1.DeploymentPlan
	if err := r.client.Get(ctx, req.NamespacedName, &plan); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.initStepStatuses(ctx, &plan); err != nil {
		return ctrl.Result{}, err
	}

	for i, stepStatus := range plan.Status.Steps {
		step := plan.Spec.Steps[i]

		switch stepStatus.Phase {
		case v1alpha1.StepReady:
			continue

		case v1alpha1.StepFailed:
			if step.Collection != nil {
				return r.reconcileCollectionStep(ctx, &plan, i)
			}
			if step.Release != nil {
				return r.reconcileReleaseStep(ctx, &plan, i, step.Release)
			}

		case v1alpha1.StepPending:
			return ctrl.Result{}, r.setStepPhase(ctx, &plan, i, v1alpha1.StepRequested, "")

		case v1alpha1.StepRequested:
			if step.Collection != nil {
				return r.reconcileCollectionStep(ctx, &plan, i)
			}
			if step.Release != nil {
				return r.reconcileReleaseStep(ctx, &plan, i, step.Release)
			}
		}
	}

	base := plan.DeepCopy()
	meta.SetStatusCondition(&plan.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionPlanReady,
		Status:             metav1.ConditionTrue,
		Reason:             "AllStepsReady",
		ObservedGeneration: plan.Generation,
	})
	if err := r.client.Status().Patch(ctx, &plan, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	r.log.Info("deployment plan ready", "name", plan.Name)
	return ctrl.Result{}, nil
}

func (r *deploymentPlanReconciler) initStepStatuses(ctx context.Context, plan *v1alpha1.DeploymentPlan) error {
	if len(plan.Status.Steps) == len(plan.Spec.Steps) {
		return nil
	}
	base := plan.DeepCopy()
	plan.Status.Steps = make([]v1alpha1.DeploymentStepStatus, len(plan.Spec.Steps))
	for i, step := range plan.Spec.Steps {
		plan.Status.Steps[i] = v1alpha1.DeploymentStepStatus{
			Name:  step.Name,
			Phase: v1alpha1.StepPending,
		}
	}
	return r.client.Status().Patch(ctx, plan, client.MergeFrom(base))
}

func (r *deploymentPlanReconciler) reconcileCollectionStep(ctx context.Context, plan *v1alpha1.DeploymentPlan, idx int) (ctrl.Result, error) {
	source := plan.Spec.Steps[idx].Collection.Source
	var rc v1alpha1.ResourceCollection
	if err := r.client.Get(ctx, client.ObjectKey{Name: source}, &rc); err != nil {
		return r.failStepWithRetry(ctx, plan, idx, fmt.Sprintf("collection %q not found", source))
	}

	cond := meta.FindStatusCondition(rc.Status.Conditions, v1alpha1.ConditionCollectionApplied)
	if cond != nil && cond.Status == metav1.ConditionFalse {
		return r.failStepWithRetry(ctx, plan, idx, fmt.Sprintf("collection %q failed: %s", source, cond.Message))
	}

	if cond == nil || cond.Status != metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.setStepPhase(ctx, plan, idx, v1alpha1.StepReady, "")
}

func (r *deploymentPlanReconciler) reconcileReleaseStep(ctx context.Context, plan *v1alpha1.DeploymentPlan, idx int, step *v1alpha1.ReleaseStep) (ctrl.Result, error) {
	var kcList v1alpha1.KitClusterList
	if err := r.client.List(ctx, &kcList); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing KitCluster: %w", err)
	}
	if len(kcList.Items) == 0 {
		return ctrl.Result{}, nil
	}
	kc := kcList.Items[0]

	all, err := r.release.All()
	if err != nil {
		return r.failStepWithRetry(ctx, plan, idx, fmt.Sprintf("loading manifests: %v", err))
	}

	_, nonCRDs := release.SplitCRDs(all)
	namespaces, resources, err := release.FilterResources(nonCRDs, kc.Spec.FeatureSet, []string{step.FilenameRegex})
	if err != nil {
		return r.failStepWithRetry(ctx, plan, idx, fmt.Sprintf("filtering manifests: %v", err))
	}

	r.log.Info("applying release step", "name", plan.Status.Steps[idx].Name, "namespaces", len(namespaces), "resources", len(resources))

	mapper, err := r.newMapper()
	if err != nil {
		return r.failStepWithRetry(ctx, plan, idx, fmt.Sprintf("discovering API: %v", err))
	}

	// TODO: add tests for partial failures, namespace-only failures, and eventual success.
	toApply := append(namespaces, resources...)
	failed := r.applyManifests(ctx, mapper, toApply)

	if failed > 0 {
		return r.failStepWithRetry(ctx, plan, idx, fmt.Sprintf("%d/%d resources failed to apply", failed, len(toApply)))
	}

	return ctrl.Result{}, r.setStepPhase(ctx, plan, idx, v1alpha1.StepReady, "")
}

func (r *deploymentPlanReconciler) applyManifests(ctx context.Context, mapper meta.RESTMapper, manifests []release.Manifest) int {
	var failed int
	for _, m := range manifests {
		if _, err := dynutil.ApplyWithMapper(ctx, r.dynClient, mapper, m.Obj); err != nil {
			r.log.Warn("failed to apply resource", "kind", m.Obj.GetKind(), "name", m.Obj.GetName(), "error", err)
			failed++
		}
	}
	return failed
}

func (r *deploymentPlanReconciler) newMapper() (meta.RESTMapper, error) {
	groupResources, err := restmapper.GetAPIGroupResources(r.discovery)
	if err != nil {
		return nil, fmt.Errorf("discovering API resources: %w", err)
	}
	return restmapper.NewDiscoveryRESTMapper(groupResources), nil
}

const stepRetryInterval = 10 * time.Second

func (r *deploymentPlanReconciler) failStepWithRetry(ctx context.Context, plan *v1alpha1.DeploymentPlan, idx int, msg string) (ctrl.Result, error) {
	if err := r.setStepPhase(ctx, plan, idx, v1alpha1.StepFailed, msg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: stepRetryInterval}, nil
}

func (r *deploymentPlanReconciler) setStepPhase(ctx context.Context, plan *v1alpha1.DeploymentPlan, idx int, phase v1alpha1.DeploymentStepPhase, message string) error {
	base := plan.DeepCopy()
	plan.Status.Steps[idx].Phase = phase
	plan.Status.Steps[idx].Message = message
	return r.client.Status().Patch(ctx, plan, client.MergeFrom(base))
}

func (r *deploymentPlanReconciler) collectionToPlan(ctx context.Context, obj client.Object) []ctrl.Request {
	var planList v1alpha1.DeploymentPlanList
	if err := r.client.List(ctx, &planList); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for _, plan := range planList.Items {
		for i, stepStatus := range plan.Status.Steps {
			if stepStatus.Phase != v1alpha1.StepRequested && stepStatus.Phase != v1alpha1.StepFailed {
				continue
			}
			if i < len(plan.Spec.Steps) && plan.Spec.Steps[i].Collection != nil && plan.Spec.Steps[i].Collection.Source == obj.GetName() {
				requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&plan)})
				break
			}
		}
	}
	return requests
}

func (r *deploymentPlanReconciler) SetupWithManager(mgr ctrl.Manager, clusterName string) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("deploymentplan-"+clusterName).
		For(&v1alpha1.DeploymentPlan{}).
		Watches(&v1alpha1.ResourceCollection{}, handler.EnqueueRequestsFromMapFunc(r.collectionToPlan)).
		Complete(r)
}
