package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/common"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	protectionFinalizer = "kit.openshift.io/cluster-protection"

	reasonReconciling     = "Reconciling"
	reasonReconcileFailed = "ReconcileFailed"
	reasonReady           = "Ready"
	reasonPulling         = "Pulling"
	reasonPulled          = "Pulled"
	reasonPullFailed      = "PullFailed"
	reasonCRDsInstalled   = "Installed"
	reasonCRDsSyncFailed  = "SyncFailed"
)

type kitClusterReconciler struct {
	client               client.Client
	crdManager           *CRDManager
	release              *release.ReleaseManifests
	apiServerURL         string
	apiServerExternalURL string
	shutdowner           common.Shutdowner
	log                  *slog.Logger
}

func (r *kitClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var kc v1alpha1.KitCluster
	if err := r.client.Get(ctx, req.NamespacedName, &kc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !kc.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &kc)
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

	if kc.Status.APIServerURL != r.apiServerURL || kc.Status.APIServerExternalURL != r.apiServerExternalURL {
		base := kc.DeepCopy()
		kc.Status.APIServerURL = r.apiServerURL
		kc.Status.APIServerExternalURL = r.apiServerExternalURL
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

	return r.phaseReady(ctx, &kc)
}

func (r *kitClusterReconciler) handleDeletion(ctx context.Context, kc *v1alpha1.KitCluster) (ctrl.Result, error) {
	r.log.Info("handling KitCluster deletion", "name", kc.Name)

	controllerutil.RemoveFinalizer(kc, protectionFinalizer)
	if err := r.client.Update(ctx, kc); err != nil {
		return ctrl.Result{}, err
	}

	r.shutdowner.Shutdown()

	r.log.Info("KitCluster deleted", "name", kc.Name)
	return ctrl.Result{}, nil
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

	if err := r.release.Load(kc.Spec.Image); err != nil {
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

	all, err := r.release.All()
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
		Complete(r)
}
