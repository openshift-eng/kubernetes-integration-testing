package kube

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

type KubeConfig struct {
	raw            api.Config
	serverOverride string
}

func New(path, serverOverride string) (*KubeConfig, error) {
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	for _, cluster := range config.Clusters {
		if err := inlineFile(&cluster.CertificateAuthority, &cluster.CertificateAuthorityData); err != nil {
			return nil, err
		}
	}
	for _, authInfo := range config.AuthInfos {
		if err := inlineFile(&authInfo.ClientCertificate, &authInfo.ClientCertificateData); err != nil {
			return nil, err
		}
		if err := inlineFile(&authInfo.ClientKey, &authInfo.ClientKeyData); err != nil {
			return nil, err
		}
	}

	return &KubeConfig{raw: *config, serverOverride: serverOverride}, nil
}

func (kc *KubeConfig) RestConfig() (*rest.Config, error) {
	clientConfig := clientcmd.NewDefaultClientConfig(kc.raw, &clientcmd.ConfigOverrides{})
	cfg, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	if kc.serverOverride != "" {
		cfg.Host = kc.serverOverride
	}
	return cfg, nil
}

func (kc *KubeConfig) WriteFlattened(path string) error {
	return clientcmd.WriteToFile(kc.raw, path)
}

func inlineFile(path *string, data *[]byte) error {
	if *path == "" || len(*data) > 0 {
		return nil
	}
	d, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *path, err)
	}
	*data = d
	*path = ""
	return nil
}
