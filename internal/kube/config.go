package kube

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func FlattenKubeconfig(path string) error {
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}

	for _, cluster := range config.Clusters {
		if err := inlineFile(&cluster.CertificateAuthority, &cluster.CertificateAuthorityData); err != nil {
			return err
		}
	}
	for _, authInfo := range config.AuthInfos {
		if err := inlineFile(&authInfo.ClientCertificate, &authInfo.ClientCertificateData); err != nil {
			return err
		}
		if err := inlineFile(&authInfo.ClientKey, &authInfo.ClientKeyData); err != nil {
			return err
		}
	}

	return clientcmd.WriteToFile(*config, path)
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

var apiServerOverride string

func SetAPIServerOverride(server string) {
	apiServerOverride = server
}

func BuildConfig(kubeconfigPath string) (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, err
	}
	if apiServerOverride != "" {
		cfg.Host = apiServerOverride
	}
	tls := &cfg.TLSClientConfig
	for _, f := range []struct {
		path *string
		data *[]byte
	}{
		{&tls.CAFile, &tls.CAData},
		{&tls.CertFile, &tls.CertData},
		{&tls.KeyFile, &tls.KeyData},
	} {
		if *f.path != "" && len(*f.data) == 0 {
			d, err := os.ReadFile(*f.path)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", *f.path, err)
			}
			*f.data = d
			*f.path = ""
		}
	}
	return cfg, nil
}
