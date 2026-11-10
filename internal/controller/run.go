package controller

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/cert"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/common"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/pod"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"
	"github.com/spf13/afero"
)

func Run(ctx context.Context, log *slog.Logger, cfg common.Config) error {
	bd, err := bootstrap.Load(ctx, cfg.KubeConfig)
	if err != nil {
		return fmt.Errorf("loading bootstrap config: %w", err)
	}

	var ps pullsecret.PullSecret
	if len(bd.PullSecret) > 0 {
		ps.Set(bd.PullSecret)
	}
	keychain, err := ps.Keychain()
	if err != nil {
		return fmt.Errorf("creating keychain: %w", err)
	}
	registry := image.NewRegistry(cfg.CacheDir, keychain, log)

	clients, err := kube.NewClients(cfg.KubeConfig)
	if err != nil {
		return fmt.Errorf("creating kube clients: %w", err)
	}

	certProvider := cert.NewProvider(clients.Clientset, bootstrap.Namespace, reconciler.NewCACallback(clients.Client, log))

	bundleStore, err := bundle.NewBundleStore(afero.NewOsFs(), filepath.Join(cfg.CacheDir, "bundles"), log)
	if err != nil {
		return fmt.Errorf("creating bundle store: %w", err)
	}
	bundleSrv := bundle.NewServer(bundleStore, cfg.BundleServerAddr, log)

	shutdowner := common.NewDeferredShutdowner(ctx)
	if err := reconciler.Start(shutdowner.Context(), log, bd, clients, cfg, registry, certProvider, shutdowner, []common.ReadyzChecker{bundleSrv}); err != nil {
		return fmt.Errorf("starting reconciler: %w", err)
	}

	go func() {
		if err := bundleSrv.Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("bundle server stopped unexpectedly", "error", err)
		}
	}()

	var runner pod.PodRunner
	if bd.KitCluster.Spec.Provider == "kwok" {
		var err error
		runner, err = runPodController(ctx, log, cfg, bd.ClusterConfig, &ps, clients)
		if err != nil {
			return fmt.Errorf("starting pod controller: %w", err)
		}
	}

	log.Info("controller started", "cluster", bd.KitCluster.Name, "provider", bd.KitCluster.Spec.Provider)
	<-ctx.Done()
	log.Info("shutting down")

	if shutdowner.IsDeleting() && runner != nil {
		runner.Teardown()
	}

	return nil
}

func runPodController(ctx context.Context, log *slog.Logger, cfg common.Config, clusterCfg *bootstrap.ClusterConfig, ps *pullsecret.PullSecret, clients *kube.Clients) (pod.PodRunner, error) {
	var podmanClient podman.Client
	if cfg.PodmanSocket != "" {
		podmanClient = podman.NewRemoteClient(cfg.PodmanSocket)
	} else {
		podmanClient = podman.NewLocalClient()
	}

	runner, err := pod.NewPodmanRunner(ctx, pod.PodmanRunnerOpts{
		PodmanClient:  podmanClient,
		Clients:       clients,
		PullSecret:    ps,
		LogsDir:       clusterCfg.LogsDir,
		VolumesDir:    clusterCfg.VolumesDir,
		Network:       clusterCfg.Network,
		APIServerHost: clusterCfg.APIServerHost,
		APIServerPort: clusterCfg.APIServerPort,
	}, log)
	if err != nil {
		return nil, err
	}

	ctrl := pod.NewController(clients, runner, log)
	go func() {
		if err := ctrl.Run(ctx); err != nil {
			log.Error("pod controller stopped", "error", err)
		}
	}()

	return runner, nil
}
