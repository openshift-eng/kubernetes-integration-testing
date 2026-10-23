package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/deployer"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/pod"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	kubeconfig := requireEnv(log, "KIT_KUBECONFIG")
	cacheDir := os.Getenv("KIT_CACHE_DIR")

	if server := os.Getenv("KIT_API_SERVER"); server != "" {
		kube.SetAPIServerOverride(server)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, kc, pullSecretData, err := bootstrap.Load(ctx, kubeconfig)
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
	registry := image.NewRegistry(cacheDir, keychain, log)

	cancel, err := reconciler.Start(ctx, log, kc, kubeconfig, cacheDir, registry)
	if err != nil {
		log.Error("starting controller manager", "error", err)
		os.Exit(1)
	}
	defer cancel()

	if kc.Spec.Provider == "kwok" {
		if err := runPodController(ctx, log, kubeconfig, cfg, &ps); err != nil {
			log.Error("starting pod controller", "error", err)
			os.Exit(1)
		}
	}

	log.Info("controller started", "cluster", kc.Name, "provider", kc.Spec.Provider)
	<-ctx.Done()
	log.Info("shutting down")
}

func newPodmanClient() podman.Client {
	if sock := os.Getenv("KIT_PODMAN_SOCKET"); sock != "" {
		return podman.NewRemoteClient(sock)
	}
	return podman.NewLocalClient()
}

func runPodController(ctx context.Context, log *slog.Logger, kubeconfig string, cfg *bootstrap.ClusterConfig, ps *pullsecret.PullSecret) error {
	changes := make(chan pod.ContainerStateChange, 100)
	runner, err := pod.NewPodmanRunner(ctx, pod.PodmanRunnerOpts{
		Client:        newPodmanClient(),
		Kubeconfig:    kubeconfig,
		PullSecret:    ps,
		LogsDir:       cfg.LogsDir,
		VolumesDir:    cfg.VolumesDir,
		Network:       cfg.Network,
		APIServerHost: cfg.APIServerHost,
		APIServerPort: cfg.APIServerPort,
	}, changes, log)
	if err != nil {
		return err
	}

	ctrl := pod.NewController(kubeconfig, runner, changes, log)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("pod controller stopped unexpectedly", "error", err)
		}
	}()

	secretProv, err := deployer.NewSecretProvisioner(kubeconfig, log)
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

func requireEnv(log *slog.Logger, key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Error(fmt.Sprintf("%s is required", key))
		os.Exit(1)
	}
	return v
}
