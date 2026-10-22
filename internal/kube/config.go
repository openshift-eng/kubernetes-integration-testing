package kube

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// BuildConfig creates a rest.Config from a kubeconfig file with TLS
// certificate data inlined from the referenced files. client-go's TLS
// transport cache keys on file paths, so recreating a cluster at the
// same path (new CA, same file name) silently reuses a stale transport.
// Inlining the data makes the cache key depend on actual content.
func BuildConfig(kubeconfigPath string) (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, err
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
