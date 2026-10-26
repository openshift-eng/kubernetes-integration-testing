package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
)

func loadConfig() (controller.Config, error) {
	kubeconfigPath := os.Getenv("KIT_KUBECONFIG")
	if kubeconfigPath == "" {
		return controller.Config{}, fmt.Errorf("KIT_KUBECONFIG is required")
	}
	kubeConfig, err := kube.New(kubeconfigPath, os.Getenv("KIT_API_SERVER"))
	if err != nil {
		return controller.Config{}, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return controller.Config{
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

	if err := controller.Run(ctx, log, cfg); err != nil {
		log.Error("controller failed", "error", err)
		os.Exit(1)
	}
}
