package kwok

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/openshift-eng/machine-config-mkit/internal/cluster"
	"github.com/openshift-eng/machine-config-mkit/internal/store"
)

const (
	clusterPrefix  = "mco-"
	defaultVersion = "0.7.0"
	releaseBaseURL = "https://github.com/kubernetes-sigs/kwok/releases/download"
)

func binID(name, version string) string {
	return fmt.Sprintf("%s-%s", name, version)
}

func registerBinaries(s store.Store, version string) {
	for _, name := range []string{"kwokctl", "kwok"} {
		n, v := name, version
		s.Register(binID(n, v), func(dest string) error {
			return downloadRelease(n, v, dest)
		})
	}
}

func downloadRelease(name, version, dest string) error {
	ver := "v" + strings.TrimPrefix(version, "v")
	url := fmt.Sprintf("%s/%s/%s-%s-%s", releaseBaseURL, ver, name, runtime.GOOS, runtime.GOARCH)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), name+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return nil
}

type provider struct {
	store    store.Store
	log      *slog.Logger
	mu       sync.Mutex
	clusters map[string]*kwokCluster
}

func NewProvider(s store.Store, log *slog.Logger) cluster.Provider {
	registerBinaries(s, defaultVersion)
	p := &provider{store: s, log: log, clusters: make(map[string]*kwokCluster)}
	p.recover()
	return p
}

func (p *provider) recover() {
	bin, err := p.kwokctl(defaultVersion)
	if err != nil {
		p.log.Warn("could not resolve kwokctl", "error", err)
		return
	}

	ctx := context.Background()
	out, err := exec.CommandContext(ctx, bin, "get", "clusters").Output()
	if err != nil {
		p.log.Warn("could not list existing kwok clusters", "error", err)
		return
	}

	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasPrefix(name, clusterPrefix) {
			continue
		}

		kubeconfig, err := getKubeConfig(ctx, bin, name)
		if err != nil {
			p.log.Warn("could not get kubeconfig", "cluster", name, "error", err)
			continue
		}

		p.clusters[name] = &kwokCluster{name: name, kubeconfig: kubeconfig, bin: bin}
		p.log.Info("recovered cluster", "name", name)
	}
}

func (p *provider) kwokctl(version string) (string, error) {
	if version == "" {
		version = defaultVersion
	}
	registerBinaries(p.store, version)
	return p.store.Get(binID("kwokctl", version))
}

func (p *provider) Get(name string) (cluster.Cluster, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clusters[prefixed(name)]
	return c, ok
}

func (p *provider) Destroy(ctx context.Context, name string, logfn func(string)) error {
	full := prefixed(name)

	p.mu.Lock()
	c, exists := p.clusters[full]
	if !exists {
		p.mu.Unlock()
		return fmt.Errorf("cluster %q not found", name)
	}
	delete(p.clusters, full)
	p.mu.Unlock()

	return c.Teardown(ctx, logfn)
}

func (p *provider) Create(ctx context.Context, opts cluster.CreateOpts, logfn func(string)) (cluster.Cluster, error) {
	full := prefixed(opts.Name)

	p.mu.Lock()
	if _, exists := p.clusters[full]; exists {
		p.mu.Unlock()
		return nil, fmt.Errorf("cluster %q already exists", opts.Name)
	}
	p.mu.Unlock()

	bin, err := p.kwokctl(opts.Version)
	if err != nil {
		return nil, err
	}

	args := []string{"create", "cluster", "--name", full, "--runtime", "binary"}
	if opts.Version != "" {
		args = append(args, "--kube-version", "v"+strings.TrimPrefix(opts.Version, "v"))
	}

	if err := run(ctx, logfn, bin, args...); err != nil {
		return nil, err
	}

	kubeconfig, err := getKubeConfig(ctx, bin, full)
	if err != nil {
		return nil, fmt.Errorf("getting kubeconfig: %w", err)
	}

	c := &kwokCluster{name: full, kubeconfig: kubeconfig, bin: bin}

	p.mu.Lock()
	p.clusters[full] = c
	p.mu.Unlock()

	return c, nil
}

func prefixed(name string) string {
	if strings.HasPrefix(name, clusterPrefix) {
		return name
	}
	return clusterPrefix + name
}

type kwokCluster struct {
	name       string
	kubeconfig string
	bin        string
}

func (c *kwokCluster) KubeConfig() string {
	return c.kubeconfig
}

func (c *kwokCluster) Teardown(ctx context.Context, logfn func(string)) error {
	return run(ctx, logfn, c.bin, "delete", "cluster", "--name", c.name)
}

func getKubeConfig(ctx context.Context, bin, name string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "get", "kubeconfig", "--name", name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func run(ctx context.Context, logfn func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}

	streamLines(stdout, logfn)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func streamLines(r io.Reader, logfn func(string)) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		logfn(s.Text())
	}
}
