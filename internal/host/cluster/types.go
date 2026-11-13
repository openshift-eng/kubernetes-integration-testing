package cluster

import (
	"context"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"
)

type ControllerConfig struct {
	Provider      string
	Network       string
	LogsDir       string
	VolumesDir    string
	APIServerHost string
	APIServerPort string
}

type Cluster interface {
	KubeConfig() string
	CAData() (certPEM, keyPEM []byte, err error)
	Teardown(ctx context.Context) error
	ControllerConfig() ControllerConfig
}

type ControllerLauncher interface {
	ResolveImage(ctx context.Context, requested string) (string, error)
	Launch(ctx context.Context, name string, c Cluster, image string) (stop func(), err error)
	WaitHealthy(ctx context.Context, name string) error
}

type Provider interface {
	Create(ctx context.Context, cluster *v1alpha1.KitCluster, ps *pullsecret.PullSecret) (Cluster, error)
	Destroy(ctx context.Context, name string) error
	Get(name string) (Cluster, bool)
	List() []string
}

type Manager interface {
	Create(ctx context.Context, kc *v1alpha1.KitCluster, pullSecretPath string) (Cluster, error)
	Destroy(ctx context.Context, name string) error
	Get(name string) (Cluster, string, bool)
	List() []string
	Recover(ctx context.Context)
}
