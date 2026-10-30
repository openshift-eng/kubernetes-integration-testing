package kwok

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/store"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	clusterPrefix  = "kit-"
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
			ver := "v" + strings.TrimPrefix(v, "v")
			url := fmt.Sprintf("%s/%s/%s-%s-%s", releaseBaseURL, ver, n, runtime.GOOS, runtime.GOARCH)
			return store.DownloadBinary(url, dest)
		})
	}
}

type provider struct {
	store    store.Store
	log      *slog.Logger
	mu       sync.Mutex
	clusters map[string]*kwokCluster
	ports    *portPool
}

func NewProvider(ctx context.Context, s store.Store, log *slog.Logger) cluster.Provider {
	registerBinaries(s, defaultVersion)
	p := &provider{store: s, log: log, clusters: make(map[string]*kwokCluster), ports: newPortPool()}
	p.recover(ctx)
	return p
}

func (p *provider) recover(ctx context.Context) {
	if !p.store.Has(binID("kwokctl", defaultVersion)) {
		return
	}
	bin, err := p.kwokctl(defaultVersion)
	if err != nil {
		p.log.Warn("could not resolve kwokctl", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "get", "clusters").Output()
	if err != nil {
		p.log.Warn("could not list existing kwok clusters", "error", err)
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

		c := &kwokCluster{name: name, kubeconfigPath: kubeconfigPath, bin: bin, store: p.store, log: p.log}
		p.clusters[name] = c
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

func (p *provider) Create(ctx context.Context, kc *v1alpha1.KitCluster, ps *pullsecret.PullSecret) (cluster.Cluster, error) {
	p.mu.Lock()
	if _, exists := p.clusters[kc.Name]; exists {
		p.mu.Unlock()
		return nil, fmt.Errorf("cluster %q already exists", kc.Name)
	}
	p.mu.Unlock()

	bin, err := p.kwokctl("")
	if err != nil {
		return nil, err
	}

	dir, err := p.store.ClusterDir(kc.Name)
	if err != nil {
		return nil, err
	}

	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating logs dir: %w", err)
	}

	kwokCfgPath := filepath.Join(dir, "kwok-config.yaml")
	if err := writeKwokConfig(kwokCfgPath, logsDir); err != nil {
		return nil, fmt.Errorf("writing kwok config: %w", err)
	}

	c, err := p.createCluster(ctx, bin, kc, dir, logsDir, kwokCfgPath)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.clusters[kc.Name] = c
	p.mu.Unlock()

	return c, nil
}

func (p *provider) createCluster(ctx context.Context, bin string, kc *v1alpha1.KitCluster, dir, logsDir, kwokCfgPath string) (*kwokCluster, error) {
	apiPort, err := p.ports.Get()
	if err != nil {
		return nil, fmt.Errorf("allocating API server port: %w", err)
	}

	c := &kwokCluster{
		name:           kc.Name,
		kubeconfigPath: filepath.Join(dir, "kubeconfig"),
		bin:            bin,
		apiPort:        apiPort,
		ports:          p.ports,
		store:          p.store,
		log:            p.log,
	}

	toolName := prefixed(kc.Name)
	// Force kubeconfig to the cluster-local path to avoid writing ~/.kube/config,
	// which causes lock contention when creating multiple clusters concurrently.
	args := []string{"create", "cluster", "--name", toolName, "--runtime", "podman", "--config", kwokCfgPath,
		"--kube-apiserver-port", strconv.Itoa(apiPort), "--kubeconfig", c.kubeconfigPath}

	if err := run(ctx, p.log, bin, args...); err != nil {
		p.ports.Release(apiPort)
		return nil, err
	}

	// TODO: kwokctl doesn't mount PKI volumes with :z, so SELinux blocks
	// access from the containers on Fedora/RHEL. Remove this workaround
	// once https://github.com/kubernetes-sigs/kwok/issues/XXX is fixed.
	fixKwokPodmanSELinux(toolName, logsDir, p.log)

	if err := ensureKubeConfig(ctx, bin, toolName, c.kubeconfigPath); err != nil {
		teardownErr := c.Teardown(ctx)
		return nil, errors.Join(fmt.Errorf("getting kubeconfig: %w", err), teardownErr)
	}

	if err := waitForAPIServer(ctx, c.kubeconfigPath, p.log); err != nil {
		teardownErr := c.Teardown(ctx)
		return nil, errors.Join(fmt.Errorf("waiting for API server: %w", err), teardownErr)
	}

	workers := kc.Spec.Workers
	if err := setupCluster(ctx, c.kubeconfigPath, workers); err != nil {
		p.log.Warn("cluster setup", "error", err)
	}

	return c, nil
}

func prefixed(name string) string {
	if strings.HasPrefix(name, clusterPrefix) {
		return name
	}
	return clusterPrefix + name
}

type kwokCluster struct {
	name           string
	kubeconfigPath string
	bin            string
	apiPort        int
	ports          *portPool
	store          store.Store
	log            *slog.Logger
}

func (c *kwokCluster) KubeConfig() string {
	return c.kubeconfigPath
}

func (c *kwokCluster) CAData() ([]byte, []byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("getting home dir: %w", err)
	}
	pkiDir := filepath.Join(home, ".kwok", "clusters", prefixed(c.name), "pki")
	certPEM, err := os.ReadFile(filepath.Join(pkiDir, "ca.crt"))
	if err != nil {
		return nil, nil, fmt.Errorf("reading CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(pkiDir, "ca.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("reading CA key: %w", err)
	}
	return certPEM, keyPEM, nil
}

func (c *kwokCluster) ControllerConfig() cluster.ControllerConfig {
	toolName := prefixed(c.name)
	dir, _ := c.store.ClusterDir(c.name)
	return cluster.ControllerConfig{
		Provider:      "kwok",
		Network:       "kwok-" + toolName,
		LogsDir:       filepath.Join(dir, "logs"),
		VolumesDir:    filepath.Join(dir, "volumes"),
		APIServerHost: "kwok-" + toolName + "-kube-apiserver",
		APIServerPort: "6443",
	}
}

func (c *kwokCluster) Teardown(ctx context.Context) error {
	if c.ports != nil {
		c.ports.Release(c.apiPort)
	}
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

func setupCluster(ctx context.Context, kubeconfigPath string, workers int) error {
	if workers <= 0 {
		workers = 1
	}

	kc, err := kube.New(kubeconfigPath, "")
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	cfg, err := kc.RestConfig()
	if err != nil {
		return fmt.Errorf("building rest config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating clientset: %w", err)
	}

	type fakeNode struct {
		name   string
		labels map[string]string
	}

	nodes := []fakeNode{
		{
			name: "kit-control-plane-0",
			labels: map[string]string{
				"type":                                  "kwok",
				"kubernetes.io/os":                      "linux",
				"kubernetes.io/arch":                    runtime.GOARCH,
				"node-role.kubernetes.io/master":        "",
				"node-role.kubernetes.io/control-plane": "",
			},
		},
	}
	for i := range workers {
		nodes = append(nodes, fakeNode{
			name: fmt.Sprintf("kit-worker-%d", i),
			labels: map[string]string{
				"type":                           "kwok",
				"kubernetes.io/os":               "linux",
				"kubernetes.io/arch":             runtime.GOARCH,
				"node-role.kubernetes.io/worker": "",
			},
		})
	}

	for _, n := range nodes {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   n.name,
				Labels: n.labels,
				Annotations: map[string]string{
					"kwok.x-k8s.io/node": "fake",
				},
			},
		}
		if _, err := clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating node %s: %w", n.name, err)
		}
	}

	if err := installLogsCRD(ctx, cfg); err != nil {
		return fmt.Errorf("installing Logs CRD: %w", err)
	}

	return nil
}

// writeKwokConfig writes the kwok configuration. It includes a custom
// pod-delete Stage so that kwokctl skips generating default pod stages
// (notably pod-ready, which would auto-set all pods to Running before
// the kit controller has a chance to start the actual containers).
// It also patches the kwok-controller component to mount the logsDir
// so that kubectl logs can read container logs from inside the container.
func writeKwokConfig(path, logsDir string) error {
	config := fmt.Sprintf(`apiVersion: config.kwok.x-k8s.io/v1alpha1
kind: KwokctlConfiguration
componentsPatches:
- name: kwok-controller
  extraVolumes:
  - name: kit-logs
    hostPath: %s
    mountPath: %s
    pathType: DirectoryOrCreate
---
apiVersion: config.kwok.x-k8s.io/v1alpha1
kind: KwokConfiguration
metadata:
  name: kwok
options:
  enableCRDs:
    - Logs
---
apiVersion: kwok.x-k8s.io/v1alpha1
kind: Stage
metadata:
  name: pod-delete
spec:
  resourceRef:
    apiGroup: v1
    kind: Pod
  selector:
    matchExpressions:
    - jq:
        key: .metadata.deletionTimestamp
        operator: Exists
  next:
    delete: true
    finalizers:
      empty: true
`, logsDir, logsDir)
	return os.WriteFile(path, []byte(config), 0o644)
}

func installLogsCRD(ctx context.Context, cfg *rest.Config) error {
	extClient, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return err
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "logs.kwok.x-k8s.io",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "kwok.x-k8s.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "Logs",
				ListKind: "LogsList",
				Plural:   "logs",
				Singular: "logs",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:                   "object",
							XPreserveUnknownFields: boolPtr(true),
						},
					},
				},
			},
		},
	}

	_, err = extClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
	return err
}

func boolPtr(b bool) *bool { return &b }

func fixKwokPodmanSELinux(clusterName, logsDir string, log *slog.Logger) {
	out, err := exec.Command("getenforce").Output()
	if err != nil || strings.TrimSpace(string(out)) == "Disabled" {
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Warn("could not determine home dir for SELinux fix", "error", err)
		return
	}
	clusterDir := filepath.Join(home, ".kwok", "clusters", clusterName)

	for _, dir := range []string{clusterDir, logsDir} {
		if out, err := exec.Command("chcon", "-R", "-t", "container_file_t", dir).CombinedOutput(); err != nil {
			log.Warn("chcon failed, containers may not start", "error", err, "path", dir, "output", string(out))
			return
		}
	}
	log.Info("relabeled dirs for SELinux", "kwokDir", clusterDir, "logsDir", logsDir)

	prefix := "kwok-" + clusterName + "-"
	for _, component := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kwok-controller"} {
		name := prefix + component
		if out, err := exec.Command("podman", "restart", name).CombinedOutput(); err != nil {
			log.Error("podman restart failed after SELinux relabel", "container", name, "error", err, "output", string(out))
			return
		}
	}
	log.Info("restarted kwok containers after SELinux fix")
}

func waitForAPIServer(ctx context.Context, kubeconfigPath string, log *slog.Logger) error {
	kc, err := kube.New(kubeconfigPath, "")
	if err != nil {
		return err
	}
	cfg, err := kc.RestConfig()
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}

	deadline := time.After(2 * time.Minute)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for API server: %w", lastErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, err := clientset.Discovery().ServerVersion()
			if err == nil {
				log.Info("API server is ready")
				return nil
			}
			lastErr = err
			log.Debug("waiting for API server", "error", err)
		}
	}
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
