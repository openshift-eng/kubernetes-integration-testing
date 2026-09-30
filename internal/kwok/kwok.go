package kwok

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
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/deployer"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/store"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
}

func NewProvider(s store.Store, log *slog.Logger) cluster.Provider {
	registerBinaries(s, defaultVersion)
	p := &provider{store: s, log: log, clusters: make(map[string]*kwokCluster)}
	p.recover()
	return p
}

func (p *provider) recover() {
	if !p.store.Has(binID("kwokctl", defaultVersion)) {
		return
	}
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

		p.clusters[name] = &kwokCluster{name: name, kubeconfigPath: kubeconfigPath, bin: bin, store: p.store, log: p.log}
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

func (p *provider) Create(ctx context.Context, opts cluster.CreateOpts) (cluster.Cluster, error) {
	p.mu.Lock()
	if _, exists := p.clusters[opts.Name]; exists {
		p.mu.Unlock()
		return nil, fmt.Errorf("cluster %q already exists", opts.Name)
	}
	p.mu.Unlock()

	bin, err := p.kwokctl(opts.Version)
	if err != nil {
		return nil, err
	}

	dir, err := p.store.ClusterDir(opts.Name)
	if err != nil {
		return nil, err
	}
	kubeconfigPath := filepath.Join(dir, "kubeconfig")

	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating logs dir: %w", err)
	}

	kwokCfgPath := filepath.Join(dir, "kwok-config.yaml")
	if err := writeKwokConfig(kwokCfgPath, logsDir); err != nil {
		return nil, fmt.Errorf("writing kwok config: %w", err)
	}

	toolName := prefixed(opts.Name)
	kwokNetwork := "kwok-" + toolName
	// kwokctl names containers as "kwok-<cluster>-<component>"
	apiServerHost := "kwok-" + toolName + "-kube-apiserver"
	apiServerPort := "6443"

	args := []string{"create", "cluster", "--name", toolName, "--runtime", "podman", "--config", kwokCfgPath}
	if opts.Version != "" {
		args = append(args, "--kube-version", "v"+strings.TrimPrefix(opts.Version, "v"))
	}

	if err := run(ctx, p.log, bin, args...); err != nil {
		return nil, err
	}

	// TODO: kwokctl doesn't mount PKI volumes with :z, so SELinux blocks
	// access from the containers on Fedora/RHEL. Remove this workaround
	// once https://github.com/kubernetes-sigs/kwok/issues/XXX is fixed.
	fixKwokPodmanSELinux(toolName, logsDir, p.log)

	if err := ensureKubeConfig(ctx, bin, toolName, kubeconfigPath); err != nil {
		return nil, fmt.Errorf("getting kubeconfig: %w", err)
	}

	if err := waitForAPIServer(ctx, kubeconfigPath, p.log); err != nil {
		return nil, fmt.Errorf("waiting for API server: %w", err)
	}

	if err := setupCluster(ctx, kubeconfigPath, opts.Workers); err != nil {
		p.log.Warn("cluster setup", "error", err)
	}

	volumesDir := filepath.Join(dir, "volumes")
	changes := make(chan ContainerStateChange, 100)

	var runner PodRunner
	runner, err = newPodmanRunner(podmanRunnerOpts{
		Kubeconfig:    kubeconfigPath,
		AuthFile:      opts.PullSecret,
		LogsDir:       logsDir,
		VolumesDir:    volumesDir,
		Network:       kwokNetwork,
		APIServerHost: apiServerHost,
		APIServerPort: apiServerPort,
	}, changes, p.log)
	if err != nil {
		_ = run(ctx, p.log, bin, "delete", "cluster", "--name", toolName)
		return nil, fmt.Errorf("creating podman runner: %w", err)
	}

	ctrl := newController(kubeconfigPath, runner, changes, p.log)

	ctrlCtx, cancelCtrl := context.WithCancel(context.Background())
	go func() {
		if err := ctrl.run(ctrlCtx); err != nil && ctrlCtx.Err() == nil {
			p.log.Error("controller stopped unexpectedly", "cluster", opts.Name, "error", err)
		}
	}()

	secretProv, err := deployer.NewSecretProvisioner(kubeconfigPath, p.log)
	if err != nil {
		p.log.Warn("failed to start secret provisioner", "error", err)
	} else {
		go func() {
			if err := secretProv.Run(ctrlCtx); err != nil && ctrlCtx.Err() == nil {
				p.log.Error("secret provisioner stopped unexpectedly", "cluster", opts.Name, "error", err)
			}
		}()
	}

	c := &kwokCluster{name: opts.Name, kubeconfigPath: kubeconfigPath, bin: bin, store: p.store, log: p.log, cancelCtrl: cancelCtrl, runner: runner}

	p.mu.Lock()
	p.clusters[opts.Name] = c
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
	name           string
	kubeconfigPath string
	bin            string
	store          store.Store
	log            *slog.Logger
	cancelCtrl     context.CancelFunc
	runner         PodRunner
}

func (c *kwokCluster) KubeConfig() string {
	return c.kubeconfigPath
}

func (c *kwokCluster) Teardown(ctx context.Context) error {
	if c.cancelCtrl != nil {
		c.cancelCtrl()
	}
	if c.runner != nil {
		c.runner.StopAll()
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

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
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
				"type":                          "kwok",
				"kubernetes.io/os":              "linux",
				"kubernetes.io/arch":            runtime.GOARCH,
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
		_ = exec.Command("podman", "restart", name).Run()
	}
	log.Info("restarted kwok containers after SELinux fix")
}

func waitForAPIServer(ctx context.Context, kubeconfigPath string, log *slog.Logger) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
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

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for API server")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, err := clientset.Discovery().ServerVersion()
			if err == nil {
				log.Info("API server is ready")
				return nil
			}
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
