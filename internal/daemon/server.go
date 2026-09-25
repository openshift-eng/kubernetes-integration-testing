package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/proto"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/cluster"
)

type server struct {
	pb.UnimplementedDaemonServer
	registry *cluster.Registry
	log      *slog.Logger
}

func (s *server) CreateCluster(req *pb.CreateClusterRequest, stream pb.Daemon_CreateClusterServer) error {
	name := req.GetName()
	s.log.Info("creating cluster", "name", name, "provider", req.GetProvider())

	if err := stream.Send(&pb.ClusterStatus{State: pb.ClusterState_CREATING}); err != nil {
		return err
	}

	opts := cluster.CreateOpts{Name: name}
	c, err := s.registry.Create(stream.Context(), req.GetProvider(), opts)
	if err != nil {
		s.log.Error("cluster creation failed", "name", name, "error", err)
		return err
	}

	return stream.Send(&pb.ClusterStatus{State: pb.ClusterState_CREATED, Kubeconfig: c.KubeConfig()})
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
