package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/pkg/naming"
)

// Cluster describes a cluster created by Create.
type Cluster struct {
	// Name is the cluster name.
	Name string
	// Kubeconfig is the path, on the daemon host, of the kubeconfig file.
	Kubeconfig string
	// AppliedResources is the number of resources requested at creation.
	// A successful Create means all of them were applied (and Ready, when
	// waiting was not disabled).
	AppliedResources int
}

// CreateOption customizes Create.
type CreateOption func(*createOptions)

type createOptions struct {
	name       string
	provider   string
	image      string
	featureSet string
	workers    int
	pullSecret string
	resources  []ResourceSource
	wait       bool
	timeout    time.Duration
}

// WithName sets the cluster name. Required.
func WithName(name string) CreateOption {
	return func(o *createOptions) { o.name = name }
}

// WithProvider selects the cluster provider (kwok, kind). Defaults to kwok.
func WithProvider(provider string) CreateOption {
	return func(o *createOptions) { o.provider = provider }
}

// WithImage sets the release image to deploy operators from.
func WithImage(image string) CreateOption {
	return func(o *createOptions) { o.image = image }
}

// WithFeatureSet sets the feature set (Default, TechPreviewNoUpgrade).
func WithFeatureSet(featureSet string) CreateOption {
	return func(o *createOptions) { o.featureSet = featureSet }
}

// WithWorkers sets the number of worker nodes. Defaults to 1.
func WithWorkers(workers int) CreateOption {
	return func(o *createOptions) { o.workers = workers }
}

// WithPullSecret sets the path of the pull secret file.
func WithPullSecret(path string) CreateOption {
	return func(o *createOptions) { o.pullSecret = path }
}

// WithResources adds sources of kit CRs to apply once the cluster is up.
// Only kit CRs are accepted; the KitCluster itself is configured through
// the other options.
func WithResources(sources ...ResourceSource) CreateOption {
	return func(o *createOptions) { o.resources = append(o.resources, sources...) }
}

// WithWait controls whether Create blocks until the applied resources
// report Ready. Defaults to true.
func WithWait(wait bool) CreateOption {
	return func(o *createOptions) { o.wait = wait }
}

// WithTimeout bounds the resource apply and wait phase. Defaults to
// 10m. Values under one second fall back to the daemon default.
func WithTimeout(d time.Duration) CreateOption {
	return func(o *createOptions) { o.timeout = d }
}

// Create creates a cluster with the given options. It blocks until the
// cluster is up, applies any requested resources (by default waiting for
// them to become Ready) and returns the cluster with its kubeconfig path.
func (c *Client) Create(ctx context.Context, opts ...CreateOption) (*Cluster, error) {
	o := createOptions{provider: "kwok", workers: 1, wait: true, timeout: 10 * time.Minute}
	for _, opt := range opts {
		opt(&o)
	}

	if o.name == "" {
		return nil, fmt.Errorf("cluster name is required")
	}
	if err := naming.ValidateClusterName(o.name); err != nil {
		return nil, err
	}

	var resources [][]byte
	for _, src := range o.resources {
		ms, err := src.Load()
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if m.Kind == "KitCluster" {
				return nil, fmt.Errorf("%s %s: the KitCluster is configured through the create options (name, provider, image, feature set, workers), not through resources", m.Kind, m.Name)
			}
			resources = append(resources, m.JSON)
		}
	}

	kc := v1alpha1.KitCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "KitCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: v1alpha1.KitClusterSpec{
			ClusterName: o.name,
			Provider:    o.provider,
			Image:       o.image,
			FeatureSet:  o.featureSet,
			Workers:     o.workers,
		},
	}
	manifest, err := json.Marshal(kc)
	if err != nil {
		return nil, fmt.Errorf("marshaling KitCluster: %w", err)
	}

	resp, err := c.pb.CreateCluster(ctx, &pb.CreateClusterRequest{
		Manifest:       manifest,
		PullSecret:     o.pullSecret,
		Resources:      resources,
		WaitResources:  o.wait,
		TimeoutSeconds: int32(o.timeout / time.Second),
	})
	if err != nil {
		return nil, err
	}

	cl := &Cluster{Name: o.name, Kubeconfig: resp.GetKubeconfig(), AppliedResources: len(resources)}
	return cl, nil
}

// Destroy destroys the cluster named name.
func (c *Client) Destroy(ctx context.Context, name string) error {
	if err := naming.ValidateClusterName(name); err != nil {
		return err
	}
	if _, err := c.pb.DestroyCluster(ctx, &pb.DestroyClusterRequest{Name: name}); err != nil {
		return err
	}
	return nil
}

// List returns the names of the clusters managed by the daemon.
func (c *Client) List(ctx context.Context) ([]string, error) {
	resp, err := c.pb.ListClusters(ctx, &pb.ListClustersRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetNames(), nil
}

// KubeconfigPath returns the path of the kubeconfig file of the cluster
// named name.
func (c *Client) KubeconfigPath(ctx context.Context, name string) (string, error) {
	resp, err := c.pb.GetClusterKubeConfig(ctx, &pb.GetClusterKubeConfigRequest{Name: name})
	if err != nil {
		return "", err
	}
	return resp.GetPath(), nil
}

// KubeconfigContent returns the content of the kubeconfig of the cluster
// named name.
func (c *Client) KubeconfigContent(ctx context.Context, name string) (string, error) {
	resp, err := c.pb.GetClusterKubeConfig(ctx, &pb.GetClusterKubeConfigRequest{Name: name, Content: true})
	if err != nil {
		return "", err
	}
	return resp.GetContent(), nil
}
