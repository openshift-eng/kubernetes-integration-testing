package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openshift-eng/kubernetes-integration-testing/pkg/client"
)

var (
	clusterProvider   string
	clusterPullSecret string
	clusterWorkers    int
	clusterImage      string
	clusterFeatureSet string
	clusterFiles      []string
	clusterNoWait     bool
	clusterTimeout    time.Duration
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
		// The resource timeout bounds apply and readiness waits; the RPC
		// gets that plus room for the cluster provisioning itself.
		ctx, cancel := context.WithTimeout(context.Background(), clusterTimeout+15*time.Minute)
		defer cancel()

		c, err := client.Connect(ctx)
		if err != nil {
			return err
		}
		defer closeDaemon(c)

		opts := []client.CreateOption{
			client.WithName(args[0]),
			client.WithProvider(clusterProvider),
			client.WithImage(clusterImage),
			client.WithFeatureSet(clusterFeatureSet),
			client.WithWorkers(clusterWorkers),
			client.WithPullSecret(clusterPullSecret),
			client.WithWait(!clusterNoWait),
			client.WithTimeout(clusterTimeout),
		}
		if len(clusterFiles) > 0 {
			opts = append(opts, client.WithResources(client.FromFiles(clusterFiles...)))
		}

		cl, err := c.Create(ctx, opts...)
		if err != nil {
			return grpcError(err)
		}

		fmt.Printf("Cluster created (kubeconfig: %s)\n", cl.Kubeconfig)
		if cl.AppliedResources > 0 {
			fmt.Printf("  applied %d kit resources\n", cl.AppliedResources)
		}
		return nil
	},
}

var clusterListCmd = &cobra.Command{
	Use:   "list",
	Short: "List clusters",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		c, err := client.Connect(ctx)
		if err != nil {
			return err
		}
		defer closeDaemon(c)

		names, err := c.List(ctx)
		if err != nil {
			return grpcError(err)
		}
		for _, name := range names {
			fmt.Println(name)
		}
		return nil
	},
}

var clusterDestroyCmd = &cobra.Command{
	Use:   "destroy [name]",
	Short: "Destroy a cluster",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		c, err := client.Connect(ctx)
		if err != nil {
			return err
		}
		defer closeDaemon(c)

		if err := c.Destroy(ctx, args[0]); err != nil {
			return grpcError(err)
		}

		fmt.Println("Cluster destroyed")
		return nil
	},
}

func closeDaemon(c *client.Client) {
	if err := c.Close(); err != nil {
		log.Warn("closing connection", "error", err)
	}
}

// grpcError renders daemon errors as user-facing messages. The client
// returns raw gRPC errors to preserve their cause; translating them for
// humans belongs to this presentation layer.
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
		ctx := context.Background()

		c, err := client.Connect(ctx)
		if err != nil {
			return err
		}
		defer closeDaemon(c)

		if kubeconfigOutput {
			content, err := c.KubeconfigContent(ctx, args[0])
			if err != nil {
				return grpcError(err)
			}
			fmt.Print(content)
			return nil
		}

		path, err := c.KubeconfigPath(ctx, args[0])
		if err != nil {
			return grpcError(err)
		}
		fmt.Println(path)
		return nil
	},
}

func init() {
	clusterCreateCmd.Flags().StringVar(&clusterProvider, "provider", "kwok", "cluster provider (kwok, kind)")
	clusterCreateCmd.Flags().StringVar(&clusterPullSecret, "pull-secret", "", "path to pull secret")
	clusterCreateCmd.Flags().IntVar(&clusterWorkers, "workers", 1, "number of worker nodes")
	clusterCreateCmd.Flags().StringVar(&clusterImage, "image", "", "release image to deploy operators from")
	clusterCreateCmd.Flags().StringVar(&clusterFeatureSet, "feature-set", "", "feature set (Default, TechPreviewNoUpgrade)")
	clusterCreateCmd.Flags().StringArrayVarP(&clusterFiles, "file", "f", nil, "kit CR manifest file, directory, or '-' for stdin; may be repeated")
	clusterCreateCmd.Flags().BoolVar(&clusterNoWait, "no-wait", false, "apply resources but do not wait for them to become ready")
	clusterCreateCmd.Flags().DurationVar(&clusterTimeout, "timeout", 10*time.Minute, "time bound for applying resources and waiting for them to become ready")
	clusterKubeconfigCmd.Flags().BoolVar(&kubeconfigOutput, "output", false, "print kubeconfig content instead of path")
	clusterCmd.AddCommand(clusterCreateCmd)
	clusterCmd.AddCommand(clusterDestroyCmd)
	clusterCmd.AddCommand(clusterListCmd)
	clusterCmd.AddCommand(clusterKubeconfigCmd)
	rootCmd.AddCommand(clusterCmd)
}
