package kind

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/store"
	"sigs.k8s.io/yaml"
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

func NewProvider(ctx context.Context, s store.Store, log *slog.Logger) cluster.Provider {
	registerBinary(s, defaultVersion)
	p := &provider{store: s, log: log, clusters: make(map[string]*kindCluster)}
	p.recover(ctx)
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

func (p *provider) recover(ctx context.Context) {
	if !p.store.Has(binID(defaultVersion)) {
		return
	}
	bin, err := p.kindBin(defaultVersion)
	if err != nil {
		p.log.Warn("could not resolve kind", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

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

func (p *provider) List() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.clusters))
	for name := range p.clusters {
		names = append(names, name)
	}
	return names
}

func (p *provider) Create(ctx context.Context, kc *v1alpha1.KitCluster, ps *pullsecret.PullSecret) (cluster.Cluster, error) {
	p.mu.Lock()
	if _, exists := p.clusters[kc.Name]; exists {
		p.mu.Unlock()
		return nil, fmt.Errorf("cluster %q already exists", kc.Name)
	}
	p.mu.Unlock()

	bin, err := p.kindBin("")
	if err != nil {
		return nil, err
	}

	dir, err := p.store.ClusterDir(kc.Name)
	if err != nil {
		return nil, err
	}
	kubeconfigPath := filepath.Join(dir, "kubeconfig")

	toolName := prefixed(kc.Name)
	args := []string{"create", "cluster", "--name", toolName, "--kubeconfig", kubeconfigPath}

	cfgPath := filepath.Join(dir, "kind-config.yaml")
	if err := writeKindConfig(cfgPath, kc.Spec.Workers, ps.Data()); err != nil {
		return nil, fmt.Errorf("writing kind config: %w", err)
	}
	args = append(args, "--config", cfgPath)

	if err := run(ctx, p.log, bin, args...); err != nil {
		return nil, err
	}

	_ = labelNodes(ctx, p.log, kubeconfigPath)

	c := &kindCluster{name: kc.Name, kubeconfigPath: kubeconfigPath, bin: bin, store: p.store, log: p.log}

	p.mu.Lock()
	p.clusters[kc.Name] = c
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
	p.mu.Unlock()

	if err := c.Teardown(ctx); err != nil {
		return err
	}

	p.mu.Lock()
	delete(p.clusters, name)
	p.mu.Unlock()
	return nil
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

func (c *kindCluster) ControllerConfig() cluster.ControllerConfig {
	return cluster.ControllerConfig{
		Provider:      "kind",
		Network:       "kind",
		APIServerHost: prefixed(c.name) + "-control-plane",
		APIServerPort: "6443",
	}
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

type kindConfig struct {
	Kind                    string     `json:"kind"`
	APIVersion              string     `json:"apiVersion"`
	ContainerdConfigPatches []string   `json:"containerdConfigPatches,omitempty"`
	Nodes                   []kindNode `json:"nodes"`
}

type kindNode struct {
	Role string `json:"role"`
}

func writeKindConfig(path string, workers int, pullSecretData []byte) error {
	cfg := kindConfig{
		Kind:       "Cluster",
		APIVersion: "kind.x-k8s.io/v1alpha4",
		Nodes:      []kindNode{{Role: "control-plane"}},
	}
	for range workers {
		cfg.Nodes = append(cfg.Nodes, kindNode{Role: "worker"})
	}

	if len(pullSecretData) > 0 {
		patch, err := containerdAuthPatch(pullSecretData)
		if err != nil {
			return fmt.Errorf("generating containerd auth patch: %w", err)
		}
		if patch != "" {
			cfg.ContainerdConfigPatches = []string{patch}
		}
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func containerdAuthPatch(data []byte) (string, error) {
	var dockerCfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &dockerCfg); err != nil {
		return "", fmt.Errorf("parsing pull secret: %w", err)
	}

	if len(dockerCfg.Auths) == 0 {
		return "", nil
	}

	var sb strings.Builder
	for registry, creds := range dockerCfg.Auths {
		if creds.Auth == "" {
			continue
		}
		fmt.Fprintf(&sb, "[plugins.\"io.containerd.grpc.v1.cri\".registry.configs.%q.auth]\n", registry)
		fmt.Fprintf(&sb, "  auth = %q\n", creds.Auth)
	}
	return sb.String(), nil
}

// TODO(pabrodri): remove once MCO migrates its nodeSelector from node-role.kubernetes.io/master to control-plane.
func labelNodes(ctx context.Context, log *slog.Logger, kubeconfigPath string) error {
	labels := map[string]map[string]string{
		"control-plane": {"node-role.kubernetes.io/master": ""},
		"worker":        {"node-role.kubernetes.io/worker": ""},
	}
	for role, lset := range labels {
		for k, v := range lset {
			args := []string{
				"--kubeconfig", kubeconfigPath,
				"label", "nodes",
				"-l", fmt.Sprintf("node-role.kubernetes.io/%s", role),
				fmt.Sprintf("%s=%s", k, v),
				"--overwrite",
			}
			cmd := exec.CommandContext(ctx, "kubectl", args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Warn("labeling nodes", "role", role, "output", string(out), "error", err)
			}
		}
	}
	return nil
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
