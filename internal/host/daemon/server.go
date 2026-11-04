package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/cluster"
)

type server struct {
	pb.UnimplementedDaemonServer
	clusterManager *cluster.ClusterManager
	log            *slog.Logger
}

func (s *server) CreateCluster(ctx context.Context, req *pb.CreateClusterRequest) (*pb.CreateClusterResponse, error) {
	var kc v1alpha1.KitCluster
	dec := json.NewDecoder(bytes.NewReader(req.GetManifest()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&kc); err != nil {
		return nil, fmt.Errorf("parsing KitCluster manifest: %w", err)
	}

	if err := cluster.ValidateClusterName(kc.Name); err != nil {
		return nil, err
	}

	// TODO: validate full KitCluster spec (provider, featureSet, workers, image, defaultCA)
	// against the CRD OpenAPI schema once the daemon accepts KIT CRs at startup.

	s.log.Info("creating cluster", "name", kc.Name, "provider", kc.Spec.Provider)

	c, err := s.clusterManager.Create(ctx, &kc, req.GetPullSecret())
	if err != nil {
		s.log.Error("cluster creation failed", "name", kc.Name, "error", err)
		return nil, err
	}

	return &pb.CreateClusterResponse{Kubeconfig: c.KubeConfig()}, nil
}

func (s *server) DestroyCluster(ctx context.Context, req *pb.DestroyClusterRequest) (*pb.DestroyClusterResponse, error) {
	name := req.GetName()
	if err := cluster.ValidateClusterName(name); err != nil {
		return nil, err
	}
	s.log.Info("destroying cluster", "name", name)

	if err := s.clusterManager.Destroy(ctx, name); err != nil {
		s.log.Error("cluster destruction failed", "name", name, "error", err)
		return nil, err
	}

	return &pb.DestroyClusterResponse{}, nil
}

func (s *server) GetClusterKubeConfig(_ context.Context, req *pb.GetClusterKubeConfigRequest) (*pb.GetClusterKubeConfigResponse, error) {
	c, _, ok := s.clusterManager.Get(req.GetName())
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
