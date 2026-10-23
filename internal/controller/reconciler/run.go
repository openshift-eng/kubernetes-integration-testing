package reconciler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/release"

	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func Start(ctx context.Context, log *slog.Logger, kc *v1alpha1.KitCluster, kubeconfig, cacheDir string, registry *image.Registry) (context.CancelFunc, error) {
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	cfg, err := kube.BuildConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}
	cfg.QPS = 50
	cfg.Burst = 100

	if err := installCRDs(ctx, log, cfg); err != nil {
		return nil, fmt.Errorf("installing CRDs: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("creating controller manager: %w", err)
	}

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}
	if err := cl.Create(ctx, kc); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating KitCluster CR: %w", err)
	}
	log.Info("ensured KitCluster CR", "name", kc.Name)

	extClient, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating apiextensions client: %w", err)
	}

	rel := release.NewReleaseManifests(registry, log)

	crdMgr := NewCRDManager(extClient.ApiextensionsV1().CustomResourceDefinitions(), log)

	kcReconciler := &kitClusterReconciler{
		client:       mgr.GetClient(),
		crdManager:   crdMgr,
		rel:          rel,
		apiServerURL: cfg.Host,
		log:          log,
	}
	if err := kcReconciler.SetupWithManager(mgr, kc.Name); err != nil {
		return nil, fmt.Errorf("setting up KitCluster controller: %w", err)
	}

	odReconciler := &operatorDeploymentReconciler{
		client:     mgr.GetClient(),
		kubeconfig: kubeconfig,
		cacheDir:   cacheDir,
		log:        log,
	}
	if err := odReconciler.SetupWithManager(mgr, kc.Name); err != nil {
		return nil, fmt.Errorf("setting up OperatorDeployment controller: %w", err)
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
