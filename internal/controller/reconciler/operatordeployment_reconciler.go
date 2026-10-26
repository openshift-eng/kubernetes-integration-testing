package reconciler

import (
	"context"
	"fmt"
	"log/slog"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/deployer"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type operatorDeploymentReconciler struct {
	client         client.Client
	kubeConfig     *kube.KubeConfig
	cacheDir       string
	collectionsDir string
	log            *slog.Logger
}

func (r *operatorDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var od v1alpha1.OperatorDeployment
	if err := r.client.Get(ctx, req.NamespacedName, &od); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if od.Status.Phase == v1alpha1.OperatorDeploymentReady {
		return ctrl.Result{}, nil
	}

	od.Status.Phase = v1alpha1.OperatorDeploymentDeploying
	od.Status.Message = ""
	if err := r.client.Status().Update(ctx, &od); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.deploy(ctx, &od); err != nil {
		od.Status.Phase = v1alpha1.OperatorDeploymentFailed
		od.Status.Message = err.Error()
		if uerr := r.client.Status().Update(ctx, &od); uerr != nil {
			r.log.Error("updating failed status", "name", od.Name, "error", uerr)
		}
		return ctrl.Result{}, err
	}

	od.Status.Phase = v1alpha1.OperatorDeploymentReady
	od.Status.Message = ""
	if err := r.client.Status().Update(ctx, &od); err != nil {
		return ctrl.Result{}, err
	}

	r.log.Info("deployed operator", "name", od.Name)
	return ctrl.Result{}, nil
}

func (r *operatorDeploymentReconciler) deploy(ctx context.Context, od *v1alpha1.OperatorDeployment) error {
	var kcList v1alpha1.KitClusterList
	if err := r.client.List(ctx, &kcList); err != nil {
		return fmt.Errorf("listing KitCluster: %w", err)
	}
	if len(kcList.Items) == 0 {
		return fmt.Errorf("no KitCluster found")
	}
	kc := kcList.Items[0]

	if kc.Spec.Image == "" {
		return fmt.Errorf("KitCluster %q has no image configured", kc.Name)
	}

	_, _, pullSecretData, err := bootstrap.Load(ctx, r.kubeConfig)
	if err != nil {
		return fmt.Errorf("loading pull secret: %w", err)
	}
	var ps pullsecret.PullSecret
	if len(pullSecretData) > 0 {
		ps.Set(pullSecretData)
	}

	keychain, err := ps.Keychain()
	if err != nil {
		return fmt.Errorf("creating keychain: %w", err)
	}

	registry := image.NewRegistry(r.cacheDir, keychain, r.log)
	d := deployer.New(registry, r.log)

	return d.Deploy(ctx, deployer.Opts{
		KubeConfig: r.kubeConfig,
		Image:      kc.Spec.Image,
		FeatureSet: kc.Spec.FeatureSet,
		Includes:   []string{od.Spec.Include},
	}, func(status string) {
		r.log.Info(status, "operator", od.Name)
	})
}

func (r *operatorDeploymentReconciler) SetupWithManager(mgr ctrl.Manager, clusterName string) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("operatordeployment-" + clusterName).
		For(&v1alpha1.OperatorDeployment{}).
		Complete(r)
}
