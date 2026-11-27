package client

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
)

// fakeDaemon is a pb.DaemonClient that records calls and returns canned
// responses.
type fakeDaemon struct {
	pb.DaemonClient

	createCalls int
	createReq   *pb.CreateClusterRequest
	createResp  *pb.CreateClusterResponse
	createErr   error

	destroyCalls int
	destroyReq   *pb.DestroyClusterRequest

	listResp *pb.ListClustersResponse
	listErr  error

	kubeconfigReq  *pb.GetClusterKubeConfigRequest
	kubeconfigResp *pb.GetClusterKubeConfigResponse
	kubeconfigErr  error
}

func (f *fakeDaemon) CreateCluster(_ context.Context, req *pb.CreateClusterRequest, _ ...grpc.CallOption) (*pb.CreateClusterResponse, error) {
	f.createCalls++
	f.createReq = req
	return f.createResp, f.createErr
}

func (f *fakeDaemon) DestroyCluster(_ context.Context, req *pb.DestroyClusterRequest, _ ...grpc.CallOption) (*pb.DestroyClusterResponse, error) {
	f.destroyCalls++
	f.destroyReq = req
	return &pb.DestroyClusterResponse{}, nil
}

func (f *fakeDaemon) ListClusters(_ context.Context, _ *pb.ListClustersRequest, _ ...grpc.CallOption) (*pb.ListClustersResponse, error) {
	return f.listResp, f.listErr
}

func (f *fakeDaemon) GetClusterKubeConfig(_ context.Context, req *pb.GetClusterKubeConfigRequest, _ ...grpc.CallOption) (*pb.GetClusterKubeConfigResponse, error) {
	f.kubeconfigReq = req
	return f.kubeconfigResp, f.kubeconfigErr
}

func newTestClient(fake *fakeDaemon) *Client {
	return &Client{pb: fake}
}

func TestCreateBuildsKitClusterRequest(t *testing.T) {
	fake := &fakeDaemon{
		createResp: &pb.CreateClusterResponse{Kubeconfig: "/tmp/kubeconfig"},
	}
	c := newTestClient(fake)

	cl, err := c.Create(context.Background(),
		WithName("my-cluster"),
		WithProvider("kind"),
		WithImage("registry.example/ocp/release:5.1"),
		WithKubeVersion("v1.36.1"),
		WithFeatureSet("TechPreviewNoUpgrade"),
		WithWorkers(3),
		WithPullSecret("/secrets/pull-secret"),
	)
	require.NoError(t, err)

	assert.Equal(t, 1, fake.createCalls)
	assert.Equal(t, "/secrets/pull-secret", fake.createReq.GetPullSecret())

	var kc v1alpha1.KitCluster
	require.NoError(t, json.Unmarshal(fake.createReq.GetManifest(), &kc))
	assert.Equal(t, "cluster", kc.Name)
	assert.Equal(t, "kit.openshift.io/v1alpha1", kc.APIVersion)
	assert.Equal(t, "KitCluster", kc.Kind)
	assert.Equal(t, "my-cluster", kc.Spec.ClusterName)
	assert.Equal(t, "kind", kc.Spec.Provider)
	assert.Equal(t, "registry.example/ocp/release:5.1", kc.Spec.Image)
	assert.Equal(t, "v1.36.1", kc.Spec.KubeVersion)
	assert.Equal(t, "TechPreviewNoUpgrade", kc.Spec.FeatureSet)
	assert.Equal(t, 3, kc.Spec.Workers)

	assert.Equal(t, &Cluster{Name: "my-cluster", Kubeconfig: "/tmp/kubeconfig"}, cl)
}

func TestCreateDefaults(t *testing.T) {
	fake := &fakeDaemon{
		createResp: &pb.CreateClusterResponse{Kubeconfig: "/tmp/kubeconfig"},
	}
	c := newTestClient(fake)

	_, err := c.Create(context.Background(), WithName("minimal"))
	require.NoError(t, err)

	var kc v1alpha1.KitCluster
	require.NoError(t, json.Unmarshal(fake.createReq.GetManifest(), &kc))
	assert.Equal(t, "kwok", kc.Spec.Provider)
	assert.Equal(t, 1, kc.Spec.Workers)
	assert.Empty(t, kc.Spec.Image)
	assert.Empty(t, kc.Spec.KubeVersion)
	assert.Empty(t, kc.Spec.FeatureSet)
}

func TestCreateKubeVersion(t *testing.T) {
	t.Run("normalizes to the canonical v-prefixed form", func(t *testing.T) {
		fake := &fakeDaemon{
			createResp: &pb.CreateClusterResponse{Kubeconfig: "/tmp/kubeconfig"},
		}
		c := newTestClient(fake)

		_, err := c.Create(context.Background(), WithName("kv"), WithKubeVersion("1.36"))
		require.NoError(t, err)

		var kc v1alpha1.KitCluster
		require.NoError(t, json.Unmarshal(fake.createReq.GetManifest(), &kc))
		assert.Equal(t, "v1.36", kc.Spec.KubeVersion)
	})

	t.Run("rejects invalid versions", func(t *testing.T) {
		fake := &fakeDaemon{}
		c := newTestClient(fake)

		_, err := c.Create(context.Background(), WithName("kv"), WithKubeVersion("1"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `invalid kube version "1"`)
		assert.Zero(t, fake.createCalls)
	})
}

func TestCreateRequiresName(t *testing.T) {
	fake := &fakeDaemon{}
	c := newTestClient(fake)

	_, err := c.Create(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster name is required")
	assert.Zero(t, fake.createCalls)
}

func TestCreateRejectsInvalidName(t *testing.T) {
	fake := &fakeDaemon{}
	c := newTestClient(fake)

	_, err := c.Create(context.Background(), WithName("kit-reserved"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not start with \"kit-\"")
	assert.Zero(t, fake.createCalls)
}

func TestCreatePreservesRPCError(t *testing.T) {
	fake := &fakeDaemon{
		createErr: status.Error(codes.Unavailable, "connection refused"),
	}
	c := newTestClient(fake)

	_, err := c.Create(context.Background(), WithName("my-cluster"))
	require.Error(t, err)

	// The gRPC cause must stay extractable for programmatic handling.
	s, ok := status.FromError(err)
	require.True(t, ok, "gRPC status must be extractable from the returned error")
	assert.Equal(t, codes.Unavailable, s.Code())
}

func TestCreateWithResources(t *testing.T) {
	fake := &fakeDaemon{
		createResp: &pb.CreateClusterResponse{Kubeconfig: "/tmp/kubeconfig"},
	}
	c := newTestClient(fake)

	cl, err := c.Create(context.Background(),
		WithName("my-cluster"),
		WithPullSecret("/secrets/pull-secret"),
		WithResources(
			FromString("apiVersion: kit.openshift.io/v1alpha1\nkind: ResourceCollection\nmetadata:\n  name: coll\nspec: {}\n"),
			FromString("apiVersion: kit.openshift.io/v1alpha1\nkind: DeploymentPlan\nmetadata:\n  name: plan\nspec:\n  steps:\n  - name: s\n    collection:\n      source: coll\n"),
		),
		WithTimeout(5*time.Minute),
	)
	require.NoError(t, err)

	// The resources travel in order, as single JSON documents.
	require.Len(t, fake.createReq.GetResources(), 2)
	assert.JSONEq(t, `{"apiVersion":"kit.openshift.io/v1alpha1","kind":"ResourceCollection","metadata":{"name":"coll"},"spec":{}}`, string(fake.createReq.GetResources()[0]))
	assert.JSONEq(t, `{"apiVersion":"kit.openshift.io/v1alpha1","kind":"DeploymentPlan","metadata":{"name":"plan"},"spec":{"steps":[{"name":"s","collection":{"source":"coll"}}]}}`, string(fake.createReq.GetResources()[1]))

	assert.True(t, fake.createReq.GetWaitResources())
	assert.EqualValues(t, 300, fake.createReq.GetTimeoutSeconds())

	// The client reports how many resources it sent.
	assert.Equal(t, 2, cl.AppliedResources)
}

func TestCreateWaitDefaults(t *testing.T) {
	fake := &fakeDaemon{
		createResp: &pb.CreateClusterResponse{Kubeconfig: "/tmp/kubeconfig"},
	}
	c := newTestClient(fake)

	_, err := c.Create(context.Background(), WithName("my-cluster"))
	require.NoError(t, err)
	assert.True(t, fake.createReq.GetWaitResources())
	assert.EqualValues(t, 600, fake.createReq.GetTimeoutSeconds())

	_, err = c.Create(context.Background(), WithName("my-cluster"), WithWait(false))
	require.NoError(t, err)
	assert.False(t, fake.createReq.GetWaitResources())
}

func TestCreateRejectsKitClusterResource(t *testing.T) {
	fake := &fakeDaemon{}
	c := newTestClient(fake)

	_, err := c.Create(context.Background(),
		WithName("my-cluster"),
		WithResources(FromString("apiVersion: kit.openshift.io/v1alpha1\nkind: KitCluster\nmetadata:\n  name: cluster\nspec:\n  clusterName: other\n  provider: kwok\n")),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configured through the create options")
	assert.Zero(t, fake.createCalls)
}

func TestCreateRejectsInvalidResource(t *testing.T) {
	fake := &fakeDaemon{}
	c := newTestClient(fake)

	_, err := c.Create(context.Background(),
		WithName("my-cluster"),
		WithResources(FromString("kind: DeploymentPlan\nmetadata:\n  name: a\n")),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing apiVersion")
	assert.Zero(t, fake.createCalls)
}

func TestDestroy(t *testing.T) {
	fake := &fakeDaemon{}
	c := newTestClient(fake)

	require.NoError(t, c.Destroy(context.Background(), "my-cluster"))
	assert.Equal(t, 1, fake.destroyCalls)
	assert.Equal(t, "my-cluster", fake.destroyReq.GetName())

	err := c.Destroy(context.Background(), "kit-reserved")
	require.Error(t, err)
	assert.Equal(t, 1, fake.destroyCalls)
}

func TestList(t *testing.T) {
	fake := &fakeDaemon{
		listResp: &pb.ListClustersResponse{Names: []string{"a", "b"}},
	}
	c := newTestClient(fake)

	names, err := c.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, names)

	fake.listErr = status.Error(codes.NotFound, "boom")
	_, err = c.List(context.Background())
	require.Error(t, err)
	s, ok := status.FromError(err)
	require.True(t, ok, "gRPC status must be extractable from the returned error")
	assert.Equal(t, codes.NotFound, s.Code())
}

func TestKubeconfig(t *testing.T) {
	fake := &fakeDaemon{
		kubeconfigResp: &pb.GetClusterKubeConfigResponse{Path: "/tmp/kubeconfig", Content: "content"},
	}
	c := newTestClient(fake)

	path, err := c.KubeconfigPath(context.Background(), "my-cluster")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/kubeconfig", path)
	assert.False(t, fake.kubeconfigReq.GetContent())

	content, err := c.KubeconfigContent(context.Background(), "my-cluster")
	require.NoError(t, err)
	assert.Equal(t, "content", content)
	assert.True(t, fake.kubeconfigReq.GetContent())
}
