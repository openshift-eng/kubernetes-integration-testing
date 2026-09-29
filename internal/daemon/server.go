package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/proto"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/deployer"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/store"
)

type server struct {
	pb.UnimplementedDaemonServer
	registry *cluster.Registry
	store    store.Store
	log      *slog.Logger
}

func (s *server) CreateCluster(req *pb.CreateClusterRequest, stream pb.Daemon_CreateClusterServer) error {
	name := req.GetName()
	s.log.Info("creating cluster", "name", name, "provider", req.GetProvider())

	if err := stream.Send(&pb.ClusterStatus{State: pb.ClusterState_CREATING}); err != nil {
		return err
	}

	opts := cluster.CreateOpts{Name: name, Workers: int(req.GetWorkers()), PullSecret: req.GetPullSecret(), Runtime: cluster.RuntimeMode(req.GetRuntime())}
	c, err := s.registry.Create(stream.Context(), req.GetProvider(), opts)
	if err != nil {
		s.log.Error("cluster creation failed", "name", name, "error", err)
		return err
	}

	if err := stream.Send(&pb.ClusterStatus{State: pb.ClusterState_CREATED, Kubeconfig: c.KubeConfig()}); err != nil {
		return err
	}

	if req.GetImage() == "" {
		return nil
	}

	if err := stream.Send(&pb.ClusterStatus{State: pb.ClusterState_DEPLOYING}); err != nil {
		return err
	}

	if err := s.deploy(stream.Context(), req, c.KubeConfig(), stream); err != nil {
		s.log.Error("deploy failed", "name", name, "error", err)
		return err
	}

	return stream.Send(&pb.ClusterStatus{State: pb.ClusterState_DEPLOYED})
}

func (s *server) deploy(ctx context.Context, req *pb.CreateClusterRequest, kubeconfig string, stream pb.Daemon_CreateClusterServer) error {
	var keychain authn.Keychain
	if req.GetPullSecret() != "" {
		var err error
		keychain, err = image.KeychainFromFile(req.GetPullSecret())
		if err != nil {
			return fmt.Errorf("loading pull secret: %w", err)
		}
	}

	reg := image.NewRegistry(s.store.ImageCacheDir(), keychain, s.log)
	dep := deployer.New(reg, s.log)

	return dep.Deploy(ctx, deployer.Opts{
		Kubeconfig: kubeconfig,
		Image:      req.GetImage(),
		FeatureSet: req.GetFeatureSet(),
		Includes:   req.GetIncludes(),
		PullSecret: req.GetPullSecret(),
	}, func(msg string) {
		_ = stream.Send(&pb.ClusterStatus{State: pb.ClusterState_DEPLOYING, Message: msg})
	})
}

func (s *server) DestroyCluster(req *pb.DestroyClusterRequest, stream pb.Daemon_DestroyClusterServer) error {
	name := req.GetName()
	s.log.Info("destroying cluster", "name", name)

	if err := stream.Send(&pb.ClusterStatus{State: pb.ClusterState_DELETING}); err != nil {
		return err
	}

	err := s.registry.Destroy(stream.Context(), name)
	if err != nil {
		s.log.Error("cluster destruction failed", "name", name, "error", err)
		return err
	}

	return stream.Send(&pb.ClusterStatus{State: pb.ClusterState_DELETED})
}

func (s *server) GetClusterKubeConfig(_ context.Context, req *pb.GetClusterKubeConfigRequest) (*pb.GetClusterKubeConfigResponse, error) {
	c, _, ok := s.registry.Get(req.GetName())
	if !ok {
		return nil, fmt.Errorf("cluster %q not found", req.GetName())
	}

	resp := &pb.GetClusterKubeConfigResponse{Path: c.KubeConfig()}
	if req.GetContent() {
		data, err := os.ReadFile(c.KubeConfig())
		if err != nil {
			return nil, fmt.Errorf("reading kubeconfig: %w", err)
		}
		resp.Content = string(data)
	}
	return resp, nil
}
