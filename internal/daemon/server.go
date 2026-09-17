package daemon

import (
	"log/slog"

	pb "github.com/openshift-eng/machine-config-mkit/api/proto"
	"github.com/openshift-eng/machine-config-mkit/internal/cluster"
)

type server struct {
	pb.UnimplementedDaemonServer
	provider cluster.Provider
	log      *slog.Logger
}

func (s *server) CreateCluster(req *pb.CreateClusterRequest, stream pb.Daemon_CreateClusterServer) error {
	name := req.GetName()
	s.log.Info("creating cluster", "name", name)

	opts := cluster.CreateOpts{Name: name}
	_, err := s.provider.Create(stream.Context(), opts, func(msg string) {
		stream.Send(&pb.LogMessage{Message: msg})
	})
	if err != nil {
		s.log.Error("cluster creation failed", "name", name, "error", err)
	}
	return err
}

func (s *server) DestroyCluster(req *pb.DestroyClusterRequest, stream pb.Daemon_DestroyClusterServer) error {
	name := req.GetName()
	s.log.Info("destroying cluster", "name", name)

	err := s.provider.Destroy(stream.Context(), name, func(msg string) {
		stream.Send(&pb.LogMessage{Message: msg})
	})
	if err != nil {
		s.log.Error("cluster destruction failed", "name", name, "error", err)
	}
	return err
}
