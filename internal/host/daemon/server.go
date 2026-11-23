package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/pkg/naming"
)

type server struct {
	pb.UnimplementedDaemonServer
	clusterManager cluster.Manager
	log            *slog.Logger
	shutdown       func()
}

func (s *server) CreateCluster(ctx context.Context, req *pb.CreateClusterRequest) (*pb.CreateClusterResponse, error) {
	var kc v1alpha1.KitCluster
	dec := json.NewDecoder(bytes.NewReader(req.GetManifest()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&kc); err != nil {
		return nil, fmt.Errorf("parsing KitCluster manifest: %w", err)
	}

	if err := naming.ValidateClusterName(kc.Spec.ClusterName); err != nil {
		return nil, err
	}

	// Resources are parsed and validated before the cluster is created,
	// so that invalid input fails without leaving a cluster behind.
	objs, err := prepareResources(req.GetResources())
	if err != nil {
		return nil, err
	}

	// TODO: validate full KitCluster spec (provider, featureSet, workers, image, defaultCA)
	// against the CRD OpenAPI schema once the daemon accepts KIT CRs at startup.

	s.log.Info("creating cluster", "name", kc.Spec.ClusterName, "provider", kc.Spec.Provider, "resources", len(objs))

	c, err := s.clusterManager.Create(ctx, &kc, req.GetPullSecret())
	if err != nil {
		s.log.Error("cluster creation failed", "name", kc.Spec.ClusterName, "error", err)
		return nil, err
	}

	if len(objs) > 0 {
		timeout := defaultResourceTimeout
		if t := req.GetTimeoutSeconds(); t > 0 {
			timeout = time.Duration(t) * time.Second
		}
		if err := applyResources(ctx, s.log, c.KubeConfig(), objs, req.GetWaitResources(), timeout); err != nil {
			// The cluster is left running for debugging on purpose: the
			// failure may be in the applied resources, and inspecting a
			// live cluster beats reading a log.
			return nil, fmt.Errorf("%w (cluster %q was left running for debugging; destroy it with `kit cluster destroy %s`)", err, kc.Spec.ClusterName, kc.Spec.ClusterName)
		}
	}

	return &pb.CreateClusterResponse{Kubeconfig: c.KubeConfig()}, nil
}

func (s *server) DestroyCluster(ctx context.Context, req *pb.DestroyClusterRequest) (*pb.DestroyClusterResponse, error) {
	name := req.GetName()
	if err := naming.ValidateClusterName(name); err != nil {
		return nil, err
	}
	s.log.Info("destroying cluster", "name", name)

	if err := s.clusterManager.Destroy(ctx, name); err != nil {
		s.log.Error("cluster destruction failed", "name", name, "error", err)
		return nil, err
	}

	return &pb.DestroyClusterResponse{}, nil
}

func (s *server) ListClusters(_ context.Context, _ *pb.ListClustersRequest) (*pb.ListClustersResponse, error) {
	return &pb.ListClustersResponse{Names: s.clusterManager.List()}, nil
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

// Shutdown asks the daemon to terminate gracefully. Asking the daemon to
// stop itself over its own protocol is the identity check: only the real
// daemon answers it. The response is delivered before the server stops
// because GracefulStop waits for in-flight RPCs.
func (s *server) Shutdown(ctx context.Context, _ *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	s.log.Info("shutdown requested by client")
	s.shutdown()
	return &pb.ShutdownResponse{}, nil
}
