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
	Launch(ctx context.Context, name string, c Cluster) (stop func(), err error)
}

type Provider interface {
	Create(ctx context.Context, cluster *v1alpha1.KitCluster, ps *pullsecret.PullSecret) (Cluster, error)
	Destroy(ctx context.Context, name string) error
	Get(name string) (Cluster, bool)
	List() []string
}
