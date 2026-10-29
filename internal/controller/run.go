package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/common"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/deployer"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/pod"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"
)

type Config struct {
	KubeConfig     *kube.KubeConfig
	CacheDir       string
	CollectionsDir string
	PodmanSocket   string
}

func Run(ctx context.Context, log *slog.Logger, cfg Config) error {
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

	shutdowner := common.NewDeferredShutdowner(ctx)
	if err := reconciler.Start(shutdowner.Context(), log, bd, clients, cfg.KubeConfig, cfg.CacheDir, cfg.CollectionsDir, registry, shutdowner); err != nil {
		return fmt.Errorf("starting reconciler: %w", err)
	}

	if bd.KitCluster.Spec.Provider == "kwok" {
		runner, err := runPodController(ctx, log, cfg, bd.ClusterConfig, &ps, clients)
		if err != nil {
			return fmt.Errorf("starting pod controller: %w", err)
		}
		shutdowner.Set(func(_ context.Context) {
			runner.Teardown()
		})
	}

	log.Info("controller started", "cluster", bd.KitCluster.Name, "provider", bd.KitCluster.Spec.Provider)
	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

func runPodController(ctx context.Context, log *slog.Logger, cfg Config, clusterCfg *bootstrap.ClusterConfig, ps *pullsecret.PullSecret, clients *kube.Clients) (pod.PodRunner, error) {
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

	secretProv, err := deployer.NewSecretProvisioner(cfg.KubeConfig, log)
	if err != nil {
		log.Warn("failed to start secret provisioner", "error", err)
	} else {
		go func() {
			if err := secretProv.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("secret provisioner stopped unexpectedly", "error", err)
			}
		}()
	}

	return runner, nil
}
