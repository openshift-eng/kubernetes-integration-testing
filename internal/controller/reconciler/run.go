package reconciler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/api/manifests"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func Start(ctx context.Context, log *slog.Logger, kitCluster *v1alpha1.KitCluster, kubeConfig *kube.KubeConfig, cacheDir, collectionsDir string, registry *image.Registry) (context.CancelFunc, error) {
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	clients, err := kube.NewClients(kubeConfig)
	if err != nil {
		return nil, err
	}

	installer := NewManifestInstaller(
		clients.Extensions.ApiextensionsV1().CustomResourceDefinitions(),
		clients.Dynamic,
		clients.Discovery,
		log,
	)
	if err := installer.Install(ctx, manifests.FS); err != nil {
		return nil, fmt.Errorf("installing manifests: %w", err)
	}

	if err := clients.Client.Create(ctx, kitCluster); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating KitCluster CR: %w", err)
	}
	log.Info("ensured KitCluster CR", "name", kitCluster.Name)

	mgr, err := ctrl.NewManager(clients.REST, ctrl.Options{
		Scheme:     kube.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("creating controller manager: %w", err)
	}

	if err := setupReconcilers(ctx, mgr, clients, kubeConfig, kitCluster.Name, cacheDir, collectionsDir, registry, log); err != nil {
		return nil, err
	}

	mgrCtx, cancel := context.WithCancel(ctx)
	go func() {
		if err := mgr.Start(mgrCtx); err != nil && mgrCtx.Err() == nil {
			log.Error("controller manager stopped unexpectedly", "error", err)
		}
	}()

	log.Info("started controller manager")
	return cancel, nil
}

func setupReconcilers(ctx context.Context, mgr ctrl.Manager, clients *kube.Clients, kubeConfig *kube.KubeConfig, clusterName, cacheDir, collectionsDir string, registry *image.Registry, log *slog.Logger) error {
	rel := release.NewReleaseManifests(registry, log)
	crdMgr := NewCRDManager(clients.Extensions.ApiextensionsV1().CustomResourceDefinitions(), log)

	reconcilers := []struct {
		name  string
		setup func() error
	}{
		{"KitCluster", func() error {
			return (&kitClusterReconciler{
				client:       mgr.GetClient(),
				crdManager:   crdMgr,
				release:      rel,
				apiServerURL: clients.REST.Host,
				log:          log,
			}).SetupWithManager(mgr, clusterName)
		}},
		{"ResourceCollection", func() error {
			return (&collectionReconciler{
				client:         mgr.GetClient(),
				dynClient:      clients.Dynamic,
				discovery:      clients.Discovery,
				release:        rel,
				collectionsDir: collectionsDir,
				log:            log,
			}).SetupWithManager(ctx, mgr, clusterName)
		}},
		{"DeploymentPlan", func() error {
			return (&deploymentPlanReconciler{
				client:    mgr.GetClient(),
				dynClient: clients.Dynamic,
				discovery: clients.Discovery,
				release:   rel,
				log:       log,
			}).SetupWithManager(mgr, clusterName)
		}},
	}

	for _, r := range reconcilers {
		if err := r.setup(); err != nil {
			return fmt.Errorf("setting up %s controller: %w", r.name, err)
		}
	}
	return nil
}
