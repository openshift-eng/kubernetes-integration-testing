package reconciler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/api/manifests"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/cert"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/common"
	kitwebhook "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/webhook"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func Start(ctx context.Context, log *slog.Logger, bd *bootstrap.BootstrapData, clients *kube.Clients, cfg common.Config, registry *image.Registry, certProvider *cert.Provider, shutdowner common.Shutdowner) error {
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	installer := NewManifestInstaller(
		clients.Extensions.ApiextensionsV1().CustomResourceDefinitions(),
		clients.Dynamic,
		clients.Discovery,
		log,
	)
	if err := installer.Install(ctx, manifests.FS); err != nil {
		return fmt.Errorf("installing manifests: %w", err)
	}

	if err := ensureBootstrapResources(ctx, clients, bd, log); err != nil {
		return fmt.Errorf("ensuring bootstrap resources: %w", err)
	}

	mgr, err := ctrl.NewManager(clients.REST, ctrl.Options{
		Scheme:                 kube.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "127.0.0.1:8081",
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		return fmt.Errorf("creating controller manager: %w", err)
	}

	if err := setupReconcilers(ctx, mgr, clients, cfg, bd, registry, certProvider, shutdowner, log); err != nil {
		return err
	}

	wh, err := kitwebhook.NewConfigurer(mgr, clients, cfg.ContainerName, certProvider, log)
	if err != nil {
		return fmt.Errorf("configuring webhook: %w", err)
	}

	if err := mgr.AddReadyzCheck("workload-overrides", wh.Ready); err != nil {
		return fmt.Errorf("adding readyz check: %w", err)
	}

	if err := wh.EnsureConfiguration(ctx); err != nil {
		return fmt.Errorf("installing webhook configuration: %w", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("controller manager stopped unexpectedly, shutting down", "error", err)
			shutdowner.Shutdown()
		}
	}()

	if err := wh.WaitReady(ctx); err != nil {
		return fmt.Errorf("waiting for webhook server: %w", err)
	}

	log.Info("started controller manager")
	return nil
}

func ensureBootstrapResources(ctx context.Context, clients *kube.Clients, bd *bootstrap.BootstrapData, log *slog.Logger) error {
	if err := clients.Client.Create(ctx, bd.KitCluster); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating KitCluster CR: %w", err)
	}
	log.Info("ensured KitCluster CR", "name", bd.KitCluster.Name)
	return nil
}

func setupReconcilers(ctx context.Context, mgr ctrl.Manager, clients *kube.Clients, cfg common.Config, bd *bootstrap.BootstrapData, registry *image.Registry, certProvider *cert.Provider, shutdowner common.Shutdowner, log *slog.Logger) error {
	rel := release.NewReleaseManifests(registry, log)
	crdMgr := NewCRDManager(clients.Extensions.ApiextensionsV1().CustomResourceDefinitions(), log)

	reconcilers := []struct {
		name  string
		setup func() error
	}{
		{"KitCluster", func() error {
			return (&kitClusterReconciler{
				client:               mgr.GetClient(),
				crdManager:           crdMgr,
				release:              rel,
				apiServerURL:         clients.REST.Host,
				apiServerExternalURL: cfg.KubeConfig.OriginalServerURL(),
				shutdowner:           shutdowner,
				log:                  log,
			}).SetupWithManager(mgr, bd.KitCluster.Name)
		}},
		{"ResourceCollection", func() error {
			return (&collectionReconciler{
				client:         mgr.GetClient(),
				dynClient:      clients.Dynamic,
				discovery:      clients.Discovery,
				release:        rel,
				collectionsDir: cfg.CollectionsDir,
				pullSecret:     string(bd.PullSecret),
				certProvider:   certProvider,
				log:            log,
			}).SetupWithManager(ctx, mgr, bd.KitCluster.Name)
		}},
		{"DeploymentPlan", func() error {
			return (&deploymentPlanReconciler{
				client:    mgr.GetClient(),
				dynClient: clients.Dynamic,
				discovery: clients.Discovery,
				release:   rel,
				log:       log,
			}).SetupWithManager(mgr, bd.KitCluster.Name)
		}},
		{"ServiceCert", func() error {
			return (&serviceReconciler{
				client:       mgr.GetClient(),
				clientset:    clients.Clientset,
				certProvider: certProvider,
				clusterName:  bd.KitCluster.Name,
				log:          log,
			}).SetupWithManager(mgr, bd.KitCluster.Name)
		}},
		{"CertificateAuthority", func() error {
			return (&caReconciler{
				client:       mgr.GetClient(),
				clientset:    clients.Clientset,
				certProvider: certProvider,
				clusterName:  bd.KitCluster.Name,
				log:          log,
			}).SetupWithManager(mgr, bd.KitCluster.Name)
		}},
	}

	for _, r := range reconcilers {
		if err := r.setup(); err != nil {
			return fmt.Errorf("setting up %s controller: %w", r.name, err)
		}
	}
	return nil
}

// TODO: propagate context through Provider (requires adding ctx to getOrCreateCA, public methods, and FuncMap)
func NewCACallback(c client.Client, log *slog.Logger) func(string, *cert.CA) {
	return func(name string, ca *cert.CA) {
		cr := &v1alpha1.CertificateAuthority{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.CertificateAuthoritySpec{
				CommonName: ca.Cert.Subject.CommonName,
				Validity:   metav1.Duration{Duration: ca.Cert.NotAfter.Sub(ca.Cert.NotBefore)},
			},
		}
		if err := c.Create(context.Background(), cr); err != nil && !apierrors.IsAlreadyExists(err) {
			log.Error("creating CertificateAuthority CR from provider callback", "ca", name, "error", err)
		} else {
			log.Info("created CertificateAuthority CR from provider callback", "ca", name)
		}
	}
}
