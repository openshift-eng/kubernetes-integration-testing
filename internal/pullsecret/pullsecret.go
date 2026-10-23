package pullsecret

import (
	"fmt"
	"os"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
)

type PullSecret struct {
	mu   sync.RWMutex
	data []byte
}

func (p *PullSecret) Set(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data = make([]byte, len(data))
	copy(p.data, data)
}

func (p *PullSecret) SetFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading pull secret: %w", err)
	}
	p.Set(data)
	return nil
}

func (p *PullSecret) Data() []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.data) == 0 {
		return nil
	}
	out := make([]byte, len(p.data))
	copy(out, p.data)
	return out
}

func (p *PullSecret) Keychain() (authn.Keychain, error) {
	data := p.Data()
	if len(data) == 0 {
		return authn.DefaultKeychain, nil
	}
	return image.KeychainFromData(data)
}

func (p *PullSecret) WithAuthFile(fn func(path string) error) error {
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
