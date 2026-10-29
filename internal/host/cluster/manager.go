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
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

type managedCluster struct {
	Cluster
	stopController func()
}

type ClusterManager struct {
	ctx             context.Context
	providers       map[string]Provider
	defaultProvider string
	launcher        ControllerLauncher
	mu              sync.Mutex
	clusters        map[string]*managedCluster
	log             *slog.Logger
}

func NewClusterManager(ctx context.Context, log *slog.Logger, defaultProvider string, providers map[string]Provider, launcher ControllerLauncher) *ClusterManager {
	cm := &ClusterManager{
		ctx:             ctx,
		providers:       providers,
		defaultProvider: defaultProvider,
		launcher:        launcher,
		clusters:        make(map[string]*managedCluster),
		log:             log,
	}
	go cm.recover(ctx)
	return cm
}

func (cm *ClusterManager) recover(ctx context.Context) {
	for _, p := range cm.providers {
		for _, name := range p.List() {
			c, ok := p.Get(name)
			if !ok {
				continue
			}
			stop, err := cm.launcher.Launch(ctx, name, c)
			if err != nil {
				cm.log.Warn("could not launch controller on recover", "cluster", name, "error", err)
				continue
			}
			cm.mu.Lock()
			cm.clusters[name] = &managedCluster{Cluster: c, stopController: stop}
			cm.mu.Unlock()
		}
	}
}

func (cm *ClusterManager) Create(ctx context.Context, kc *v1alpha1.KitCluster, pullSecretPath string) (Cluster, error) {
	if _, _, ok := cm.Get(kc.Name); ok {
		return nil, fmt.Errorf("cluster %q already exists", kc.Name)
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
		destroyErr := p.Destroy(ctx, kc.Name)
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
		destroyErr := p.Destroy(ctx, kc.Name)
		return nil, errors.Join(fmt.Errorf("storing bootstrap config: %w", err), destroyErr)
	}

	stop, err := cm.launcher.Launch(cm.ctx, kc.Name, c)
	if err != nil {
		destroyErr := p.Destroy(ctx, kc.Name)
		return nil, errors.Join(fmt.Errorf("launching controller: %w", err), destroyErr)
	}

	if err := cm.waitForController(ctx, clusterKC, kc.Name); err != nil {
		stop()
		destroyErr := p.Destroy(ctx, kc.Name)
		return nil, errors.Join(fmt.Errorf("waiting for controller readiness: %w", err), destroyErr)
	}

	mc := &managedCluster{Cluster: c, stopController: stop}
	cm.mu.Lock()
	cm.clusters[kc.Name] = mc
	cm.mu.Unlock()
	return mc, nil
}

func (cm *ClusterManager) Destroy(ctx context.Context, name string) error {
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

func (cm *ClusterManager) deleteKitCluster(ctx context.Context, c Cluster, name string) error {
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

	if err := dynClient.Resource(kitClusterGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("deleting KitCluster CR: %w", err)
	}

	cm.log.Info("waiting for KitCluster deletion (finalizer teardown)", "cluster", name)
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := dynClient.Resource(kitClusterGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return true, nil
		}
		return false, nil
	})
}

func (cm *ClusterManager) Get(name string) (Cluster, string, bool) {
	for pname, p := range cm.providers {
		if c, ok := p.Get(name); ok {
			return c, pname, true
		}
	}
	return nil, "", false
}

var kitClusterGVR = schema.GroupVersionResource{
	Group:    v1alpha1.GroupVersion.Group,
	Version:  v1alpha1.GroupVersion.Version,
	Resource: "kitclusters",
}

func (cm *ClusterManager) waitForController(ctx context.Context, kc *kube.KubeConfig, crName string) error {
	cfg, err := kc.RestConfig()
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := dynClient.Resource(kitClusterGVR).Get(ctx, crName, metav1.GetOptions{})
		return err == nil, nil
	}); err != nil {
		return fmt.Errorf("waiting for KitCluster CR %q: %w", crName, err)
	}

	cm.log.Info("controller ready", "cluster", crName)
	return nil
}

func (cm *ClusterManager) provider(name string) (Provider, error) {
	if name == "" {
		name = cm.defaultProvider
	}
	p, ok := cm.providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	return p, nil
}

