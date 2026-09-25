package kind

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/store"
)

const (
	clusterPrefix  = "kit-"
	defaultVersion = "0.27.0"
	releaseBaseURL = "https://github.com/kubernetes-sigs/kind/releases/download"
)

type provider struct {
	store    store.Store
	log      *slog.Logger
	mu       sync.Mutex
	clusters map[string]*kindCluster
}

func NewProvider(s store.Store, log *slog.Logger) cluster.Provider {
	registerBinary(s, defaultVersion)
	p := &provider{store: s, log: log, clusters: make(map[string]*kindCluster)}
	p.recover()
	return p
}

func registerBinary(s store.Store, version string) {
	s.Register(binID(version), func(dest string) error {
		ver := "v" + strings.TrimPrefix(version, "v")
		url := fmt.Sprintf("%s/%s/kind-%s-%s", releaseBaseURL, ver, runtime.GOOS, runtime.GOARCH)
		return store.DownloadBinary(url, dest)
	})
}

func binID(version string) string {
	return fmt.Sprintf("kind-%s", version)
}



func (p *provider) kindBin(version string) (string, error) {
	if version == "" {
		version = defaultVersion
	}
	registerBinary(p.store, version)
	return p.store.Get(binID(version))
}

func (p *provider) recover() {
	if !p.store.Has(binID(defaultVersion)) {
		return
	}
	bin, err := p.kindBin(defaultVersion)
	if err != nil {
		p.log.Warn("could not resolve kind", "error", err)
		return
	}

	ctx := context.Background()
	out, err := exec.CommandContext(ctx, bin, "get", "clusters").Output()
	if err != nil {
		p.log.Warn("could not list existing kind clusters", "error", err)
		return
	}

	for _, toolName := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		toolName = strings.TrimSpace(toolName)
		if toolName == "" || !strings.HasPrefix(toolName, clusterPrefix) {
			continue
		}
		name := strings.TrimPrefix(toolName, clusterPrefix)

		kubeconfigPath := p.kubeconfigPath(name)
		if err := ensureKubeConfig(ctx, bin, toolName, kubeconfigPath); err != nil {
			p.log.Warn("could not ensure kubeconfig", "cluster", name, "error", err)
			continue
		}

		p.clusters[name] = &kindCluster{name: name, kubeconfigPath: kubeconfigPath, bin: bin, store: p.store, log: p.log}
		p.log.Info("recovered cluster", "name", name)
	}
}

func (p *provider) Get(name string) (cluster.Cluster, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clusters[name]
	return c, ok
}

func (p *provider) Create(ctx context.Context, opts cluster.CreateOpts) (cluster.Cluster, error) {
	p.mu.Lock()
	if _, exists := p.clusters[opts.Name]; exists {
		p.mu.Unlock()
		return nil, fmt.Errorf("cluster %q already exists", opts.Name)
	}
	p.mu.Unlock()

	bin, err := p.kindBin(opts.Version)
	if err != nil {
		return nil, err
	}

	dir, err := p.store.ClusterDir(opts.Name)
	if err != nil {
		return nil, err
	}
	kubeconfigPath := filepath.Join(dir, "kubeconfig")

	toolName := prefixed(opts.Name)
	args := []string{"create", "cluster", "--name", toolName, "--kubeconfig", kubeconfigPath}
	if opts.Version != "" {
		args = append(args, "--image", fmt.Sprintf("kindest/node:v%s", strings.TrimPrefix(opts.Version, "v")))
	}

	if err := run(ctx, p.log, bin, args...); err != nil {
		return nil, err
	}

	c := &kindCluster{name: opts.Name, kubeconfigPath: kubeconfigPath, bin: bin, store: p.store, log: p.log}

	p.mu.Lock()
	p.clusters[opts.Name] = c
	p.mu.Unlock()

	return c, nil
}

func (p *provider) Destroy(ctx context.Context, name string) error {
	p.mu.Lock()
	c, exists := p.clusters[name]
	if !exists {
		p.mu.Unlock()
		return fmt.Errorf("cluster %q not found", name)
	}
	delete(p.clusters, name)
	p.mu.Unlock()

	return c.Teardown(ctx)
}

func prefixed(name string) string {
	if strings.HasPrefix(name, clusterPrefix) {
		return name
	}
	return clusterPrefix + name
}

type kindCluster struct {
	name           string
	kubeconfigPath string
	bin            string
	store          store.Store
	log            *slog.Logger
}

func (c *kindCluster) KubeConfig() string {
	return c.kubeconfigPath
}

func (c *kindCluster) Teardown(ctx context.Context) error {
	if err := run(ctx, c.log, c.bin, "delete", "cluster", "--name", prefixed(c.name)); err != nil {
		return err
	}
	return c.store.RemoveClusterDir(c.name)
}

func ensureKubeConfig(ctx context.Context, bin, name, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, bin, "get", "kubeconfig", "--name", name).Output()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

func (p *provider) kubeconfigPath(name string) string {
	dir, _ := p.store.ClusterDir(name)
	return filepath.Join(dir, "kubeconfig")
}

func run(ctx context.Context, log *slog.Logger, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Info(string(out))
	}
	if err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}
