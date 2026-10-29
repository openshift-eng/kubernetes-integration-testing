package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
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
	clusterCfg, kitCluster, pullSecretData, err := bootstrap.Load(ctx, cfg.KubeConfig)
	if err != nil {
		return fmt.Errorf("loading bootstrap config: %w", err)
	}

	var ps pullsecret.PullSecret
	if len(pullSecretData) > 0 {
		ps.Set(pullSecretData)
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

	cancel, err := reconciler.Start(ctx, log, kitCluster, clients, cfg.KubeConfig, cfg.CacheDir, cfg.CollectionsDir, registry)
	if err != nil {
		return fmt.Errorf("starting reconciler: %w", err)
	}
	defer cancel()

	if kitCluster.Spec.Provider == "kwok" {
		if err := runPodController(ctx, log, cfg, clusterCfg, &ps, clients); err != nil {
			return fmt.Errorf("starting pod controller: %w", err)
		}
	}

	log.Info("controller started", "cluster", kitCluster.Name, "provider", kitCluster.Spec.Provider)
	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

func runPodController(ctx context.Context, log *slog.Logger, cfg Config, clusterCfg *bootstrap.ClusterConfig, ps *pullsecret.PullSecret, clients *kube.Clients) error {
	var podmanClient podman.Client
	if cfg.PodmanSocket != "" {
		podmanClient = podman.NewRemoteClient(cfg.PodmanSocket)
	} else {
		podmanClient = podman.NewLocalClient()
	}

	changes := make(chan pod.ContainerStateChange, 100)
	runner, err := pod.NewPodmanRunner(ctx, pod.PodmanRunnerOpts{
		PodmanClient:  podmanClient,
		Clients:       clients,
		PullSecret:    ps,
		LogsDir:       clusterCfg.LogsDir,
		VolumesDir:    clusterCfg.VolumesDir,
		Network:       clusterCfg.Network,
		APIServerHost: clusterCfg.APIServerHost,
		APIServerPort: clusterCfg.APIServerPort,
	}, changes, log)
	if err != nil {
		return err
	}

	ctrl := pod.NewController(clients, runner, changes, log)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("pod controller stopped unexpectedly", "error", err)
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

	return nil
}
