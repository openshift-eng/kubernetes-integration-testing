package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/workload/override"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// podDisposition is the reconciler's verdict on a matched pod.
type podDisposition int

const (
	podApplied     podDisposition = iota // spec already satisfies the override
	podPending                           // needs delete-to-recreate
	podInvalid                           // override doesn't fit pod containers
	podUnsupported                       // no controller owner or nodeName constraint
	podSuperseded                        // a more specific override wins
)

type classifiedPod struct {
	pod         *corev1.Pod
	disposition podDisposition
	winner      string // winning override name; only set when superseded
}

// overrideEvaluation classifies pods for one WorkloadOverride.
// Counts form a funnel: matched ⊇ selected ⊇ applied.
type overrideEvaluation struct {
	matched  int32
	selected int32
	applied  int32
	pods     []classifiedPod
}

type overrideState int

const (
	overrideStatePending overrideState = iota
	overrideStateApplied
	overrideStateInvalid
)

type workloadOverrideReconciler struct {
	client client.Client
	log    *slog.Logger
}

func NewWorkloadOverrideReconciler(client client.Client, log *slog.Logger) *workloadOverrideReconciler {
	return &workloadOverrideReconciler{client: client, log: log}
}

func (r *workloadOverrideReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var wo v1alpha1.WorkloadOverride
	if err := r.client.Get(ctx, req.NamespacedName, &wo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var allOverrides v1alpha1.WorkloadOverrideList
	if err := r.client.List(ctx, &allOverrides); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing overrides: %w", err)
	}

	listOpts := []client.ListOption{}
	if wo.Spec.Match.Namespace != "" {
		listOpts = append(listOpts, client.InNamespace(wo.Spec.Match.Namespace))
	}
	var pods corev1.PodList
	if err := r.client.List(ctx, &pods, listOpts...); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing pods: %w", err)
	}

	eval := evaluatePods(&wo, allOverrides.Items, pods.Items)

	var errs []error
	for _, cp := range eval.pods {
		if cp.disposition != podPending || cp.pod.DeletionTimestamp != nil {
			continue
		}
		r.log.Info("deleting pod missing override",
			"pod", cp.pod.Name, "namespace", cp.pod.Namespace, "override", wo.Name)
		if err := r.client.Delete(ctx, cp.pod); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting pod %s/%s: %w", cp.pod.Namespace, cp.pod.Name, err))
		}
	}

	base := wo.DeepCopy()
	wo.Status.MatchedPods = eval.matched
	wo.Status.AppliedPods = eval.applied
	meta.SetStatusCondition(&wo.Status.Conditions, buildCondition(eval, wo.Generation, errs))

	if err := r.client.Status().Patch(ctx, &wo, client.MergeFrom(base)); err != nil {
		errs = append(errs, err)
	}

	return ctrl.Result{}, errors.Join(errs...)
}

func evaluatePods(wo *v1alpha1.WorkloadOverride, allOverrides []v1alpha1.WorkloadOverride, pods []corev1.Pod) overrideEvaluation {
	var eval overrideEvaluation

	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if !override.MatchesPod(wo.Spec.Match, pod, pod.Namespace) {
			continue
		}

		eval.matched++

		winner := override.SelectMatch(allOverrides, pod, pod.Namespace)
		if winner == nil || winner.Name != wo.Name {
			winnerName := ""
			if winner != nil {
				winnerName = winner.Name
			}
			eval.pods = append(eval.pods, classifiedPod{pod: pod, disposition: podSuperseded, winner: winnerName})
			continue
		}

		eval.selected++

		switch checkOverride(pod, wo) {
		case overrideStateApplied:
			eval.applied++
			eval.pods = append(eval.pods, classifiedPod{pod: pod, disposition: podApplied})
		case overrideStateInvalid:
			eval.pods = append(eval.pods, classifiedPod{pod: pod, disposition: podInvalid})
		default:
			disp := podPending
			if !canRecreatePod(pod, wo) {
				disp = podUnsupported
			}
			eval.pods = append(eval.pods, classifiedPod{pod: pod, disposition: disp})
		}
	}

	return eval
}

func buildCondition(eval overrideEvaluation, generation int64, deleteErrors []error) metav1.Condition {
	var hasInvalid, hasUnsupported bool
	supersededBy := map[string]struct{}{}
	for _, cp := range eval.pods {
		switch cp.disposition {
		case podInvalid:
			hasInvalid = true
		case podUnsupported:
			hasUnsupported = true
		case podSuperseded:
			if cp.winner != "" {
				supersededBy[cp.winner] = struct{}{}
			}
		default:
			continue
		}
	}

	cond := metav1.Condition{
		Type:               v1alpha1.ConditionOverrideApplied,
		ObservedGeneration: generation,
	}
	switch {
	case len(deleteErrors) > 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Failed"
		cond.Message = errors.Join(deleteErrors...).Error()
	case hasInvalid:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Invalid"
		cond.Message = "override does not apply to one or more matched pod specs"
	case hasUnsupported:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "RecreationUnsupported"
		cond.Message = "one or more matched pods cannot be safely recreated"
	case eval.applied < eval.selected:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PendingApplication"
		cond.Message = fmt.Sprintf("%d/%d selected pods have the override", eval.applied, eval.selected)
		if eval.matched > eval.selected {
			cond.Message += "; " + supersededMessage(eval.matched, eval.selected, supersededBy)
		}
	case eval.matched > eval.selected:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "SupersededOverride"
		cond.Message = supersededMessage(eval.matched, eval.selected, supersededBy)
	case eval.matched == 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NoMatch"
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Applied"
		cond.Message = fmt.Sprintf("%d/%d matching pods use the override", eval.applied, eval.matched)
	}
	return cond
}

func canRecreatePod(pod *corev1.Pod, wo *v1alpha1.WorkloadOverride) bool {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return false
	}
	if wo.Spec.Match.NodeName != "" && owner.Kind != "DaemonSet" {
		return false
	}
	return true
}

func supersededMessage(matched, selected int32, supersededBy map[string]struct{}) string {
	winners := make([]string, 0, len(supersededBy))
	for name := range supersededBy {
		winners = append(winners, name)
	}
	sort.Strings(winners)
	return fmt.Sprintf(
		"%d/%d matching pods superseded by %s",
		matched-selected,
		matched,
		strings.Join(winners, ", "),
	)
}

func checkOverride(pod *corev1.Pod, wo *v1alpha1.WorkloadOverride) overrideState {
	if wo.Spec.NoOp {
		if override.IsNoOp(pod) {
			return overrideStateApplied
		}
		return overrideStatePending
	}
	mutated := pod.DeepCopy()
	outcome, _ := override.ApplyContainerOverrides(mutated, wo.Spec.Containers)
	if outcome == override.Invalid {
		return overrideStateInvalid
	}
	if equality.Semantic.DeepEqual(pod.Spec, mutated.Spec) {
		return overrideStateApplied
	}
	return overrideStatePending
}

func (r *workloadOverrideReconciler) allOverrides(ctx context.Context, _ client.Object) []ctrl.Request {
	var overrides v1alpha1.WorkloadOverrideList
	if err := r.client.List(ctx, &overrides); err != nil {
		r.log.Error("listing overrides for fan-out", "error", err)
		return nil
	}
	requests := make([]ctrl.Request, len(overrides.Items))
	for i, wo := range overrides.Items {
		requests[i] = ctrl.Request{NamespacedName: client.ObjectKey{Name: wo.Name}}
	}
	return requests
}

// podSpecChanged filters pod updates to changes that affect override evaluation:
// spec, labels, ownerRefs, deletion, or terminal phase transition.
// Status-only updates (conditions, IPs, container statuses) are discarded.
func podSpecChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(_ event.CreateEvent) bool { return true },
		DeleteFunc: func(_ event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return true
			}
			if !equality.Semantic.DeepEqual(oldPod.Spec, newPod.Spec) {
				return true
			}
			if !equality.Semantic.DeepEqual(oldPod.Labels, newPod.Labels) {
				return true
			}
			if !equality.Semantic.DeepEqual(oldPod.OwnerReferences, newPod.OwnerReferences) {
				return true
			}
			if (oldPod.DeletionTimestamp == nil) != (newPod.DeletionTimestamp == nil) {
				return true
			}
			oldTerminal := oldPod.Status.Phase == corev1.PodSucceeded || oldPod.Status.Phase == corev1.PodFailed
			newTerminal := newPod.Status.Phase == corev1.PodSucceeded || newPod.Status.Phase == corev1.PodFailed
			return oldTerminal != newTerminal
		},
		GenericFunc: func(_ event.GenericEvent) bool { return true },
	}
}

func (r *workloadOverrideReconciler) SetupWithManager(mgr ctrl.Manager) error {
	genChanged := predicate.GenerationChangedPredicate{}
	return ctrl.NewControllerManagedBy(mgr).
		Named("workloadoverride").
		For(&v1alpha1.WorkloadOverride{}, builder.WithPredicates(genChanged)).
		// Fan-out: a spec change on any override may shift priority for all others.
		Watches(&v1alpha1.WorkloadOverride{}, handler.EnqueueRequestsFromMapFunc(r.allOverrides),
			builder.WithPredicates(genChanged)).
		// Any relevant pod change re-evaluates all overrides (few, cluster-scoped).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.allOverrides),
			builder.WithPredicates(podSpecChanged())).
		Complete(r)
}
