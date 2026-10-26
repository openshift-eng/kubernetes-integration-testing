package daemon

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
)

func NewClient(sockPath string) (pb.DaemonClient, *grpc.ClientConn, error) {
	conn, err := grpc.NewClient(
		"passthrough:///unix",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return net.DialTimeout("unix", sockPath, 3*time.Second)
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to daemon: %w", err)
	}

	return pb.NewDaemonClient(conn), conn, nil
}
