package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	protectionFinalizer = "kit.openshift.io/cluster-protection"

	reasonReconciling          = "Reconciling"
	reasonReconcileFailed      = "ReconcileFailed"
	reasonReady                = "Ready"
	reasonPulling              = "Pulling"
	reasonPulled               = "Pulled"
	reasonPullFailed           = "PullFailed"
	reasonCRDsInstalled        = "Installed"
	reasonCRDsSyncFailed       = "SyncFailed"
	reasonCollectionsApplied   = "Applied"
	reasonCollectionsPending   = "Pending"
	reasonCollectionsNotFound  = "NotFound"
)

type kitClusterReconciler struct {
	client       client.Client
	crdManager   *CRDManager
	rel          *release.ReleaseManifests
	apiServerURL string
	log          *slog.Logger
}

func (r *kitClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var kc v1alpha1.KitCluster
	if err := r.client.Get(ctx, req.NamespacedName, &kc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !controllerutil.ContainsFinalizer(&kc, protectionFinalizer) {
		controllerutil.AddFinalizer(&kc, protectionFinalizer)
		if err := r.client.Update(ctx, &kc); err != nil {
			r.log.Error("adding finalizer", "name", kc.Name, "error", err)
			return ctrl.Result{}, err
		}
	}

	readyCond := meta.FindStatusCondition(kc.Status.Conditions, v1alpha1.ConditionReady)
	if readyCond != nil && readyCond.Status == metav1.ConditionTrue && readyCond.ObservedGeneration == kc.Generation {
		return ctrl.Result{}, nil
	}

	if kc.Status.APIServerURL != r.apiServerURL {
		base := kc.DeepCopy()
		kc.Status.APIServerURL = r.apiServerURL
		if err := r.client.Status().Patch(ctx, &kc, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	if kc.Spec.Image != "" {
		result, err := r.phaseImagePull(ctx, &kc)
		if result != nil {
			return *result, err
		}

		result, err = r.phaseCRDSync(ctx, &kc)
		if result != nil {
			return *result, err
		}
	}

	if len(kc.Spec.Collections) > 0 {
		result, err := r.phaseCollections(ctx, &kc)
		if result != nil {
			return *result, err
		}
	}

	return r.phaseReady(ctx, &kc)
}

// phaseImagePull pulls the release image synchronously. Returns nil result to
// signal "phase already done, continue to next". Each invocation does at most
// one status write.
func (r *kitClusterReconciler) phaseImagePull(ctx context.Context, kc *v1alpha1.KitCluster) (*ctrl.Result, error) {
	cond := meta.FindStatusCondition(kc.Status.Conditions, v1alpha1.ConditionImagePulled)
	if cond != nil && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == kc.Generation {
		return nil, nil
	}

	if cond == nil || cond.Reason != reasonPulling || cond.ObservedGeneration != kc.Generation {
		r.log.Info("pulling release image", "name", kc.Name, "image", kc.Spec.Image)
		base := kc.DeepCopy()
		r.setCondition(kc, v1alpha1.ConditionImagePulled, metav1.ConditionFalse, reasonPulling, fmt.Sprintf("Pulling image %s", kc.Spec.Image))
		r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconciling, "Pulling release image")
		if err := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); err != nil {
			return &ctrl.Result{}, err
		}
		return &ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}

	if err := r.rel.Load(kc.Spec.Image); err != nil {
		base := kc.DeepCopy()
		r.setCondition(kc, v1alpha1.ConditionImagePulled, metav1.ConditionFalse, reasonPullFailed, err.Error())
		r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconcileFailed, fmt.Sprintf("Failed to pull image: %s", err))
		if statusErr := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); statusErr != nil {
			r.log.Error("updating status after pull failure", "name", kc.Name, "error", statusErr)
		}
		// Returning the error triggers controller-runtime's exponential backoff for retries.
		return &ctrl.Result{}, fmt.Errorf("loading release image: %w", err)
	}

	r.log.Info("release image ready", "name", kc.Name, "image", kc.Spec.Image)

	base := kc.DeepCopy()
	r.setCondition(kc, v1alpha1.ConditionImagePulled, metav1.ConditionTrue, reasonPulled, "")
	r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconciling, "Image pulled, syncing CRDs")
	if err := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); err != nil {
		return &ctrl.Result{}, err
	}

	return &ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

// phaseCRDSync applies and deletes CRDs from the release. Returns nil result
// to signal "phase already done, continue to next".
func (r *kitClusterReconciler) phaseCRDSync(ctx context.Context, kc *v1alpha1.KitCluster) (*ctrl.Result, error) {
	cond := meta.FindStatusCondition(kc.Status.Conditions, v1alpha1.ConditionCRDsInstalled)
	if cond != nil && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == kc.Generation {
		return nil, nil
	}

	r.log.Info("syncing CRDs", "name", kc.Name)

	all, err := r.rel.All()
	if err != nil {
		return &ctrl.Result{}, fmt.Errorf("parsing manifests: %w", err)
	}
	crds, _ := release.SplitCRDs(all)

	if err := r.crdManager.Sync(ctx, crds, kc.Spec.FeatureSet); err != nil {
		base := kc.DeepCopy()
		r.setCondition(kc, v1alpha1.ConditionCRDsInstalled, metav1.ConditionFalse, reasonCRDsSyncFailed, err.Error())
		r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconcileFailed, fmt.Sprintf("Failed to sync CRDs: %s", err))
		if statusErr := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); statusErr != nil {
			r.log.Error("updating status after CRD sync failure", "name", kc.Name, "error", statusErr)
		}
		// Returning the error triggers controller-runtime's exponential backoff for retries.
		return &ctrl.Result{}, fmt.Errorf("syncing CRDs: %w", err)
	}

	r.log.Info("CRDs synced", "name", kc.Name)

	base := kc.DeepCopy()
	r.setCondition(kc, v1alpha1.ConditionCRDsInstalled, metav1.ConditionTrue, reasonCRDsInstalled, "")
	r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconciling, "CRDs installed")
	if err := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); err != nil {
		return &ctrl.Result{}, err
	}

	return &ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *kitClusterReconciler) phaseCollections(ctx context.Context, kc *v1alpha1.KitCluster) (*ctrl.Result, error) {
	cond := meta.FindStatusCondition(kc.Status.Conditions, v1alpha1.ConditionCollectionsApplied)
	if cond != nil && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == kc.Generation {
		return nil, nil
	}

	var pending []string
	var missing []string
	for _, name := range kc.Spec.Collections {
		var rc v1alpha1.ResourceCollection
		if err := r.client.Get(ctx, client.ObjectKey{Name: name}, &rc); err != nil {
			missing = append(missing, name)
			continue
		}
		rcCond := meta.FindStatusCondition(rc.Status.Conditions, v1alpha1.ConditionCollectionApplied)
		if rcCond == nil || rcCond.Status != metav1.ConditionTrue {
			pending = append(pending, name)
		}
	}

	if len(missing) > 0 {
		msg := fmt.Sprintf("collections not found: %s", strings.Join(missing, ", "))
		base := kc.DeepCopy()
		r.setCondition(kc, v1alpha1.ConditionCollectionsApplied, metav1.ConditionFalse, reasonCollectionsNotFound, msg)
		r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconcileFailed, msg)
		if err := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); err != nil {
			return &ctrl.Result{}, err
		}
		return &ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if len(pending) > 0 {
		msg := fmt.Sprintf("waiting for collections: %s", strings.Join(pending, ", "))
		base := kc.DeepCopy()
		r.setCondition(kc, v1alpha1.ConditionCollectionsApplied, metav1.ConditionFalse, reasonCollectionsPending, msg)
		r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconciling, msg)
		if err := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); err != nil {
			return &ctrl.Result{}, err
		}
		return &ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	base := kc.DeepCopy()
	r.setCondition(kc, v1alpha1.ConditionCollectionsApplied, metav1.ConditionTrue, reasonCollectionsApplied, "")
	r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconciling, "Collections applied")
	if err := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); err != nil {
		return &ctrl.Result{}, err
	}

	return nil, nil
}

func (r *kitClusterReconciler) phaseReady(ctx context.Context, kc *v1alpha1.KitCluster) (ctrl.Result, error) {
	base := kc.DeepCopy()
	r.setCondition(kc, v1alpha1.ConditionReady, metav1.ConditionTrue, reasonReady, "")
	if err := r.client.Status().Patch(ctx, kc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	r.log.Info("reconciled KitCluster", "name", kc.Name)
	return ctrl.Result{}, nil
}

func (r *kitClusterReconciler) setCondition(kc *v1alpha1.KitCluster, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&kc.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: kc.Generation,
	})
}

func (r *kitClusterReconciler) SetupWithManager(mgr ctrl.Manager, clusterName string) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("kitcluster-"+clusterName).
		For(&v1alpha1.KitCluster{}).
		// Re-evaluate KitCluster readiness when a referenced collection changes phase.
		Watches(&v1alpha1.ResourceCollection{}, handler.EnqueueRequestsFromMapFunc(r.collectionsToKitCluster)).
		Complete(r)
}

// collectionsToKitCluster maps a ResourceCollection event to the KitClusters that reference it.
func (r *kitClusterReconciler) collectionsToKitCluster(ctx context.Context, obj client.Object) []ctrl.Request {
	var kcList v1alpha1.KitClusterList
	if err := r.client.List(ctx, &kcList); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for _, kc := range kcList.Items {
		for _, ref := range kc.Spec.Collections {
			if ref == obj.GetName() {
				requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&kc)})
				break
			}
		}
	}
	return requests
}
