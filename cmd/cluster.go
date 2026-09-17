package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	pb "github.com/openshift-eng/machine-config-mkit/api/proto"
	"github.com/openshift-eng/machine-config-mkit/internal/daemon"
	"github.com/openshift-eng/machine-config-mkit/internal/store"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		defer conn.Close()

		stream, err := client.CreateCluster(context.Background(), &pb.CreateClusterRequest{Name: args[0]})
		if err != nil {
			return grpcError(err)
		}

		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return grpcError(err)
			}
			fmt.Println(prettyLog(msg.GetMessage()))
		}
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
		defer conn.Close()

		stream, err := client.DestroyCluster(context.Background(), &pb.DestroyClusterRequest{Name: args[0]})
		if err != nil {
			return grpcError(err)
		}

		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return grpcError(err)
			}
			fmt.Println(prettyLog(msg.GetMessage()))
		}
	},
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

func prettyLog(line string) string {
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return line
	}

	msg, _ := entry["msg"].(string)
	cluster, _ := entry["cluster"].(string)

	var parts []string
	if cluster != "" {
		parts = append(parts, fmt.Sprintf("[%s]", cluster))
	}
	parts = append(parts, msg)
	if elapsed, ok := entry["elapsed"].(map[string]any); ok {
		if human, ok := elapsed["human"].(string); ok {
			parts = append(parts, fmt.Sprintf("elapsed=%s", human))
		}
	}
	return strings.Join(parts, " ")
}

func init() {
	clusterCmd.AddCommand(clusterCreateCmd)
	clusterCmd.AddCommand(clusterDestroyCmd)
	rootCmd.AddCommand(clusterCmd)
}
