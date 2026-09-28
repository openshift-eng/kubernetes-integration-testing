package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/proto"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/daemon"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/store"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	clusterProvider   string
	clusterImage      string
	clusterPullSecret string
	clusterFeatureSet string
	clusterIncludes   []string
	clusterWorkers    int32
)

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Manage clusters",
}

var stateMessages = map[pb.ClusterState]string{
	pb.ClusterState_CREATING:  "Creating cluster...",
	pb.ClusterState_CREATED:   "Cluster created",
	pb.ClusterState_DELETING:  "Deleting cluster...",
	pb.ClusterState_DELETED:   "Cluster deleted",
	pb.ClusterState_DEPLOYING: "Deploying...",
	pb.ClusterState_DEPLOYED:  "Deploy complete",
}

var clusterCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a cluster",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, conn, err := daemonClient()
		if err != nil {
			return err
		}
		defer func() {
			if err := conn.Close(); err != nil {
				log.Warn("closing connection", "error", err)
			}
		}()

		stream, err := client.CreateCluster(context.Background(), &pb.CreateClusterRequest{
			Name:       args[0],
			Provider:   clusterProvider,
			Image:      clusterImage,
			PullSecret: clusterPullSecret,
			FeatureSet: clusterFeatureSet,
			Includes:   clusterIncludes,
			Workers:    clusterWorkers,
		})
		if err != nil {
			return grpcError(err)
		}

		return printStatusStream(stream)
	},
}

var clusterDestroyCmd = &cobra.Command{
	Use:   "destroy [name]",
	Short: "Destroy a cluster",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, conn, err := daemonClient()
		if err != nil {
			return err
		}
		defer func() {
			if err := conn.Close(); err != nil {
				log.Warn("closing connection", "error", err)
			}
		}()

		stream, err := client.DestroyCluster(context.Background(), &pb.DestroyClusterRequest{Name: args[0]})
		if err != nil {
			return grpcError(err)
		}

		return printStatusStream(stream)
	},
}

type statusReceiver interface {
	Recv() (*pb.ClusterStatus, error)
}

func printStatusStream(stream statusReceiver) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return grpcError(err)
		}
		message, ok := stateMessages[msg.GetState()]
		if !ok {
			message = msg.GetState().String()
		}
		if m := msg.GetMessage(); m != "" {
			message += fmt.Sprintf(": %s", m)
		}
		if kc := msg.GetKubeconfig(); kc != "" {
			message += fmt.Sprintf(" (kubeconfig: %s)", kc)
		}
		fmt.Println(message)
	}
}

func ensureDaemon() error {
	if _, running := store.IsRunning(appStore.PidPath(), appStore.SocketPath()); running {
		return nil
	}

	log.Warn("not running, starting...")
	if err := runBackground(); err != nil {
		return fmt.Errorf("could not start daemon: %w", err)
	}

	sock := appStore.SocketPath()
	for range 30 {
		if _, err := os.Stat(sock); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon started but socket not ready")
}

func daemonClient() (pb.DaemonClient, *grpc.ClientConn, error) {
	if err := ensureDaemon(); err != nil {
		return nil, nil, err
	}
	return daemon.NewClient(appStore)
}

func grpcError(err error) error {
	if s, ok := status.FromError(err); ok {
		if s.Code() == codes.Unavailable {
			return fmt.Errorf("lost connection to daemon")
		}
		return fmt.Errorf("%s", s.Message())
	}
	return err
}

var kubeconfigOutput bool

var clusterKubeconfigCmd = &cobra.Command{
	Use:           "kubeconfig [name]",
	Short:         "Print the kubeconfig path for a cluster",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, conn, err := daemonClient()
		if err != nil {
			return err
		}
		defer func() {
			if err := conn.Close(); err != nil {
				log.Warn("closing connection", "error", err)
			}
		}()

		resp, err := client.GetClusterKubeConfig(context.Background(), &pb.GetClusterKubeConfigRequest{
			Name:    args[0],
			Content: kubeconfigOutput,
		})
		if err != nil {
			return grpcError(err)
		}

		if kubeconfigOutput {
			fmt.Print(resp.GetContent())
		} else {
			fmt.Println(resp.GetPath())
		}
		return nil
	},
}

func init() {
	clusterCreateCmd.Flags().StringVar(&clusterProvider, "provider", "kwok", "cluster provider (kwok, kind)")
	clusterCreateCmd.Flags().StringVar(&clusterImage, "image", "", "release image to deploy CRDs and resources from")
	clusterCreateCmd.Flags().StringVar(&clusterPullSecret, "pull-secret", "", "path to pull secret for the release image")
	clusterCreateCmd.Flags().StringVar(&clusterFeatureSet, "feature-set", "", "feature set for manifest filtering (Default, TechPreviewNoUpgrade)")
	clusterCreateCmd.Flags().StringSliceVar(&clusterIncludes, "include", nil, "regex patterns for non-CRD manifests to include")
	clusterCreateCmd.Flags().Int32Var(&clusterWorkers, "workers", 1, "number of worker nodes (kind provider only)")
	clusterKubeconfigCmd.Flags().BoolVar(&kubeconfigOutput, "output", false, "print kubeconfig content instead of path")
	clusterCmd.AddCommand(clusterCreateCmd)
	clusterCmd.AddCommand(clusterDestroyCmd)
	clusterCmd.AddCommand(clusterKubeconfigCmd)
	rootCmd.AddCommand(clusterCmd)
}
