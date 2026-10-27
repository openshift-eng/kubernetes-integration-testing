package reconciler

import (
	"context"
	"log/slog"
	"slices"
	"sort"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const collectionConfigName = "cluster"

type collectionConfigReconciler struct {
	client client.Client
	log    *slog.Logger
}

func (r *collectionConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cc v1alpha1.CollectionConfig
	if err := r.client.Get(ctx, client.ObjectKey{Name: collectionConfigName}, &cc); err != nil {
		if apierrors.IsNotFound(err) {
			cc = v1alpha1.CollectionConfig{
				ObjectMeta: metav1.ObjectMeta{Name: collectionConfigName},
			}
			if err := r.client.Create(ctx, &cc); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			return ctrl.Result{}, err
		}
	}

	var rcList v1alpha1.ResourceCollectionList
	if err := r.client.List(ctx, &rcList); err != nil {
		return ctrl.Result{}, err
	}

	available := make([]string, 0, len(rcList.Items))
	for _, rc := range rcList.Items {
		available = append(available, rc.Name)
	}
	sort.Strings(available)

	if slices.Equal(cc.Status.Available, available) {
		return ctrl.Result{}, nil
	}

	base := cc.DeepCopy()
	cc.Status.Available = available
	if err := r.client.Status().Patch(ctx, &cc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	r.log.Info("updated CollectionConfig", "available", available)
	return ctrl.Result{}, nil
}

func (r *collectionConfigReconciler) SetupWithManager(mgr ctrl.Manager, clusterName string) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("collectionconfig-"+clusterName).
		For(&v1alpha1.CollectionConfig{}).
		Watches(&v1alpha1.ResourceCollection{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: collectionConfigName}}}
			},
		)).
		Complete(r)
}
