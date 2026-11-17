package common

import (
	"fmt"
	"os"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
)

type Config struct {
	KubeConfig       *kube.KubeConfig
	ContainerName    string
	CacheDir         string
	CollectionsDir   string
	PodmanSocket     string
	BundleServerAddr string
}

func LoadConfig() (Config, error) {
	kubeconfigPath := os.Getenv("KIT_KUBECONFIG")
	if kubeconfigPath == "" {
		return Config{}, fmt.Errorf("KIT_KUBECONFIG is required")
	}
	kubeConfig, err := kube.New(kubeconfigPath, os.Getenv("KIT_API_SERVER"))
	if err != nil {
		return Config{}, fmt.Errorf("loading kubeconfig: %w", err)
	}

	containerName := os.Getenv("KIT_CONTAINER_NAME")
	if containerName == "" {
		containerName, _ = os.Hostname()
	}

	bundleAddr := os.Getenv("KIT_BUNDLE_SERVER_ADDR")
	if bundleAddr == "" {
		bundleAddr = ":8082"
	}

	return Config{
		KubeConfig:       kubeConfig,
		ContainerName:    containerName,
		CacheDir:         os.Getenv("KIT_CACHE_DIR"),
		CollectionsDir:   os.Getenv("KIT_COLLECTIONS_DIR"),
		PodmanSocket:     os.Getenv("KIT_PODMAN_SOCKET"),
		BundleServerAddr: bundleAddr,
	}, nil
}
