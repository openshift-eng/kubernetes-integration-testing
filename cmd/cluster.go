package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemon"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	clusterProvider   string
	clusterPullSecret string
	clusterWorkers    int
	clusterImage      string
	clusterFeatureSet string
)

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Manage clusters",
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

		kc := v1alpha1.KitCluster{
			TypeMeta: metav1.TypeMeta{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "KitCluster",
			},
			ObjectMeta: metav1.ObjectMeta{Name: args[0]},
			Spec: v1alpha1.KitClusterSpec{
				Provider:   clusterProvider,
				Image:      clusterImage,
				FeatureSet: clusterFeatureSet,
				Workers:    clusterWorkers,
			},
		}
		manifest, err := json.Marshal(kc)
		if err != nil {
			return fmt.Errorf("marshaling KitCluster: %w", err)
		}

		resp, err := client.CreateCluster(context.Background(), &pb.CreateClusterRequest{
			Manifest:   manifest,
			PullSecret: clusterPullSecret,
		})
		if err != nil {
			return grpcError(err)
		}

		fmt.Printf("Cluster created (kubeconfig: %s)\n", resp.GetKubeconfig())
		return nil
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

		if _, err := client.DestroyCluster(context.Background(), &pb.DestroyClusterRequest{Name: args[0]}); err != nil {
			return grpcError(err)
		}

		fmt.Println("Cluster destroyed")
		return nil
	},
}

func ensureDaemon() error {
	f, err := os.OpenFile(appPaths.Lock(), os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening lock file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("acquiring lock: %w", err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	if _, running := daemon.IsRunning(appFs, appPaths.Pid(), appPaths.Socket()); running {
		return nil
	}

	log.Warn("not running, starting...")
	if err := runBackground(); err != nil {
		return fmt.Errorf("could not start daemon: %w", err)
	}

	sock := appPaths.Socket()
	for range 600 {
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
	return daemon.NewClient(appPaths.Socket())
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
	clusterCreateCmd.Flags().StringVar(&clusterPullSecret, "pull-secret", "", "path to pull secret")
	clusterCreateCmd.Flags().IntVar(&clusterWorkers, "workers", 1, "number of worker nodes")
	clusterCreateCmd.Flags().StringVar(&clusterImage, "image", "", "release image to deploy operators from")
	clusterCreateCmd.Flags().StringVar(&clusterFeatureSet, "feature-set", "", "feature set (Default, TechPreviewNoUpgrade)")
	clusterKubeconfigCmd.Flags().BoolVar(&kubeconfigOutput, "output", false, "print kubeconfig content instead of path")
	clusterCmd.AddCommand(clusterCreateCmd)
	clusterCmd.AddCommand(clusterDestroyCmd)
	clusterCmd.AddCommand(clusterKubeconfigCmd)
	rootCmd.AddCommand(clusterCmd)
}
