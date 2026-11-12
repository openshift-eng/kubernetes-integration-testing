package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/naming"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

type managedCluster struct {
	Cluster
	stopController func()
}

type clusterManager struct {
	ctx             context.Context
	providers       map[string]Provider
	defaultProvider string
	launcher        ControllerLauncher
	mu              sync.Mutex
	clusters        map[string]*managedCluster
	clusterLocks    map[string]*sync.Mutex
	log             *slog.Logger
}

func NewClusterManager(ctx context.Context, log *slog.Logger, defaultProvider string, providers map[string]Provider, launcher ControllerLauncher) Manager {
	return &clusterManager{
		ctx:             ctx,
		providers:       providers,
		defaultProvider: defaultProvider,
		launcher:        launcher,
		clusters:        make(map[string]*managedCluster),
		clusterLocks:    make(map[string]*sync.Mutex),
		log:             log,
	}
}

// TODO: check if the controller container is already running before re-launching;
// --replace kills a healthy controller unnecessarily on daemon restart.
func (cm *clusterManager) Recover(ctx context.Context) {
	for _, p := range cm.providers {
		for _, name := range p.List() {
			if ctx.Err() != nil {
				cm.log.Warn("recover aborted", "cluster", name, "error", ctx.Err())
				return
			}
			c, ok := p.Get(name)
			if !ok {
				continue
			}
			cm.log.Info("recovering cluster", "cluster", name)
			stop, err := cm.launcher.Launch(ctx, name, c)
			if err != nil {
				cm.log.Warn("could not launch controller on recover", "cluster", name, "error", err)
				continue
			}
			if err := cm.launcher.WaitHealthy(ctx, name); err != nil {
				cm.log.Warn("controller not healthy after recover", "cluster", name, "error", err)
				stop()
				continue
			}
			cm.mu.Lock()
			cm.clusters[name] = &managedCluster{Cluster: c, stopController: stop}
			cm.mu.Unlock()
			cm.log.Info("cluster recovered", "cluster", name)
		}
	}
}

func (cm *clusterManager) clusterLock(name string) *sync.Mutex {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	mu, ok := cm.clusterLocks[name]
	if !ok {
		mu = &sync.Mutex{}
		cm.clusterLocks[name] = mu
	}
	return mu
}

func (cm *clusterManager) Create(ctx context.Context, kc *v1alpha1.KitCluster, pullSecretPath string) (Cluster, error) {
	name := kc.Spec.ClusterName
	if err := naming.ValidateClusterName(name); err != nil {
		return nil, err
	}

	mu := cm.clusterLock(name)
	mu.Lock()
	defer mu.Unlock()

	if _, _, ok := cm.Get(name); ok {
		return nil, fmt.Errorf("cluster %q already exists", name)
	}

	p, err := cm.provider(kc.Spec.Provider)
	if err != nil {
		return nil, err
	}

	var ps pullsecret.PullSecret
	if pullSecretPath != "" {
		if err := ps.SetFromFile(pullSecretPath); err != nil {
			return nil, err
		}
	}

	c, err := p.Create(ctx, kc, &ps)
	if err != nil {
		return nil, err
	}

	clusterKC, err := kube.New(c.KubeConfig(), "")
	if err != nil {
		destroyErr := p.Destroy(ctx, name)
		return nil, errors.Join(fmt.Errorf("loading cluster kubeconfig: %w", err), destroyErr)
	}

	var caCertPEM, caKeyPEM []byte
	if certPEM, keyPEM, err := c.CAData(); err != nil {
		cm.log.Warn("could not extract CA data from provider, controller will generate self-signed", "error", err)
	} else {
		caCertPEM, caKeyPEM = certPEM, keyPEM
	}

	ccfg := c.ControllerConfig()
	if err := bootstrap.Store(ctx, clusterKC, bootstrap.ClusterConfig{
		Network:       ccfg.Network,
		LogsDir:       ccfg.LogsDir,
		VolumesDir:    ccfg.VolumesDir,
		APIServerHost: ccfg.APIServerHost,
		APIServerPort: ccfg.APIServerPort,
	}, kc, ps.Data(), caCertPEM, caKeyPEM); err != nil {
		destroyErr := p.Destroy(ctx, name)
		return nil, errors.Join(fmt.Errorf("storing bootstrap config: %w", err), destroyErr)
	}

	stop, err := cm.launcher.Launch(cm.ctx, name, c)
	if err != nil {
		destroyErr := p.Destroy(ctx, name)
		return nil, errors.Join(fmt.Errorf("launching controller: %w", err), destroyErr)
	}

	if err := cm.launcher.WaitHealthy(ctx, name); err != nil {
		stop()
		destroyErr := p.Destroy(ctx, name)
		return nil, errors.Join(fmt.Errorf("waiting for controller readiness: %w", err), destroyErr)
	}

	mc := &managedCluster{Cluster: c, stopController: stop}
	cm.mu.Lock()
	cm.clusters[name] = mc
	cm.mu.Unlock()
	return mc, nil
}

func (cm *clusterManager) Destroy(ctx context.Context, name string) error {
	if err := naming.ValidateClusterName(name); err != nil {
		return err
	}

	mu := cm.clusterLock(name)
	mu.Lock()
	defer mu.Unlock()

	cm.mu.Lock()
	mc, ok := cm.clusters[name]
	if ok {
		delete(cm.clusters, name)
	}
	cm.mu.Unlock()

	if ok {
		if err := cm.deleteKitCluster(ctx, mc.Cluster, name); err != nil {
			cm.log.Warn("failed to delete KitCluster CR, proceeding with teardown", "cluster", name, "error", err)
		}
		if mc.stopController != nil {
			mc.stopController()
		}
	}

	for _, p := range cm.providers {
		if _, ok := p.Get(name); ok {
			return p.Destroy(ctx, name)
		}
	}
	return fmt.Errorf("cluster %q not found", name)
}

func (cm *clusterManager) deleteKitCluster(ctx context.Context, c Cluster, name string) error {
	clusterKC, err := kube.New(c.KubeConfig(), "")
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}

	cfg, err := clusterKC.RestConfig()
	if err != nil {
		return fmt.Errorf("building rest config: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	if err := dynClient.Resource(kitClusterGVR).Delete(ctx, "cluster", metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("deleting KitCluster CR: %w", err)
	}

	cm.log.Info("waiting for KitCluster deletion (finalizer teardown)", "cluster", name)
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := dynClient.Resource(kitClusterGVR).Get(ctx, "cluster", metav1.GetOptions{})
		if err != nil {
			return true, nil
		}
		return false, nil
	})
}

func (cm *clusterManager) Get(name string) (Cluster, string, bool) {
	for pname, p := range cm.providers {
		if c, ok := p.Get(name); ok {
			return c, pname, true
		}
	}
	return nil, "", false
}

func (cm *clusterManager) List() []string {
	all := sets.New[string]()
	for _, p := range cm.providers {
		all.Insert(p.List()...)
	}
	return sets.List(all)
}

var kitClusterGVR = schema.GroupVersionResource{
	Group:    v1alpha1.GroupVersion.Group,
	Version:  v1alpha1.GroupVersion.Version,
	Resource: "kitclusters",
}

func (cm *clusterManager) provider(name string) (Provider, error) {
	if name == "" {
		name = cm.defaultProvider
	}
	p, ok := cm.providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	return p, nil
}
