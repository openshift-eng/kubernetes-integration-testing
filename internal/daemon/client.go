package daemon

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/openshift-eng/machine-config-mkit/api/proto"
	"github.com/openshift-eng/machine-config-mkit/internal/store"
)

func NewClient(s store.Store) (pb.DaemonClient, *grpc.ClientConn, error) {
	sock := s.SocketPath()

	conn, err := grpc.NewClient(
		"passthrough:///unix",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return net.DialTimeout("unix", sock, 3*time.Second)
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to daemon: %w", err)
	}

	return pb.NewDaemonClient(conn), conn, nil
}
