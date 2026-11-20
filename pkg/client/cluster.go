package client

import (
	"context"
	"encoding/json"
	"fmt"

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

// Create creates a cluster with the given options. It blocks until the
// cluster is up and returns it with its kubeconfig path.
func (c *Client) Create(ctx context.Context, opts ...CreateOption) (*Cluster, error) {
	o := createOptions{provider: "kwok", workers: 1}
	for _, opt := range opts {
		opt(&o)
	}

	if o.name == "" {
		return nil, fmt.Errorf("cluster name is required")
	}
	if err := naming.ValidateClusterName(o.name); err != nil {
		return nil, err
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
		Manifest:   manifest,
		PullSecret: o.pullSecret,
	})
	if err != nil {
		return nil, err
	}

	return &Cluster{Name: o.name, Kubeconfig: resp.GetKubeconfig()}, nil
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
