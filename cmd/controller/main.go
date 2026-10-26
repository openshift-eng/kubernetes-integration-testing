package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/deployer"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/pod"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"
)

type controllerConfig struct {
	KubeConfig     *kube.KubeConfig
	CacheDir       string
	CollectionsDir string
	PodmanSocket   string
}

func loadConfig() (controllerConfig, error) {
	kubeconfigPath := os.Getenv("KIT_KUBECONFIG")
	if kubeconfigPath == "" {
		return controllerConfig{}, fmt.Errorf("KIT_KUBECONFIG is required")
	}
	kubeConfig, err := kube.New(kubeconfigPath, os.Getenv("KIT_API_SERVER"))
	if err != nil {
		return controllerConfig{}, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return controllerConfig{
		KubeConfig:     kubeConfig,
		CacheDir:       os.Getenv("KIT_CACHE_DIR"),
		CollectionsDir: os.Getenv("KIT_COLLECTIONS_DIR"),
		PodmanSocket:   os.Getenv("KIT_PODMAN_SOCKET"),
	}, nil
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := loadConfig()
	if err != nil {
		log.Error("loading config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	clusterCfg, kitCluster, pullSecretData, err := bootstrap.Load(ctx, cfg.KubeConfig)
	if err != nil {
		log.Error("loading bootstrap config", "error", err)
		os.Exit(1)
	}

	var ps pullsecret.PullSecret
	if len(pullSecretData) > 0 {
		ps.Set(pullSecretData)
	}
	keychain, err := ps.Keychain()
	if err != nil {
		log.Error("creating keychain", "error", err)
		os.Exit(1)
	}
	registry := image.NewRegistry(cfg.CacheDir, keychain, log)

	cancel, err := reconciler.Start(ctx, log, kitCluster, cfg.KubeConfig, cfg.CacheDir, cfg.CollectionsDir, registry)
	if err != nil {
		log.Error("starting controller manager", "error", err)
		os.Exit(1)
	}
	defer cancel()

	if kitCluster.Spec.Provider == "kwok" {
		if err := runPodController(ctx, log, cfg, clusterCfg, &ps); err != nil {
			log.Error("starting pod controller", "error", err)
			os.Exit(1)
		}
	}

	log.Info("controller started", "cluster", kitCluster.Name, "provider", kitCluster.Spec.Provider)
	<-ctx.Done()
	log.Info("shutting down")
}

func runPodController(ctx context.Context, log *slog.Logger, cfg controllerConfig, clusterCfg *bootstrap.ClusterConfig, ps *pullsecret.PullSecret) error {
	var podmanClient podman.Client
	if cfg.PodmanSocket != "" {
		podmanClient = podman.NewRemoteClient(cfg.PodmanSocket)
	} else {
		podmanClient = podman.NewLocalClient()
	}

	changes := make(chan pod.ContainerStateChange, 100)
	runner, err := pod.NewPodmanRunner(ctx, pod.PodmanRunnerOpts{
		Client:        podmanClient,
		KubeConfig:    cfg.KubeConfig,
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

	ctrl := pod.NewController(cfg.KubeConfig, runner, changes, log)
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
