package kwok

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type PullSecretProvider struct {
	mu   sync.RWMutex
	data []byte
}

func (p *PullSecretProvider) Set(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data = make([]byte, len(data))
	copy(p.data, data)
}

func (p *PullSecretProvider) SetFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading pull secret: %w", err)
	}
	p.Set(data)
	return nil
}

func (p *PullSecretProvider) RecoverFromCluster(ctx context.Context, kubeconfigPath string) error {
	cfg, err := kube.BuildConfig(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating clientset: %w", err)
	}
	secret, err := clientset.CoreV1().Secrets("openshift-config").Get(ctx, "pull-secret", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting pull-secret: %w", err)
	}
	data, ok := secret.Data[".dockerconfigjson"]
	if !ok || len(data) == 0 {
		return nil
	}
	p.Set(data)
	return nil
}

func (p *PullSecretProvider) Data() []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.data) == 0 {
		return nil
	}
	out := make([]byte, len(p.data))
	copy(out, p.data)
	return out
}

func (p *PullSecretProvider) WithAuthFile(fn func(path string) error) error {
	p.mu.RLock()
	data := p.data
	p.mu.RUnlock()

	if len(data) == 0 {
		return fn("")
	}

	f, err := os.CreateTemp("", "pull-secret-*.json")
	if err != nil {
		return fmt.Errorf("creating temp auth file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("setting temp auth file permissions: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing temp auth file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp auth file: %w", err)
	}

	return fn(f.Name())
}
