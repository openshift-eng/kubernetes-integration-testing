package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"

	pb "github.com/openshift-eng/machine-config-mkit/api/proto"
	"github.com/openshift-eng/machine-config-mkit/internal/cluster"
	"github.com/openshift-eng/machine-config-mkit/internal/store"
	"google.golang.org/grpc"
)

type ProviderFactory func(log *slog.Logger) cluster.Provider

func Run(ctx context.Context, s store.Store, factory ProviderFactory, foreground bool) error {
	logFile, err := os.OpenFile(s.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	var w io.Writer = logFile
	if foreground {
		w = io.MultiWriter(logFile, os.Stderr)
	}
	handler := slog.NewTextHandler(w, nil)
	slog.SetDefault(slog.New(handler))

	log := slog.New(handler).With("module", "daemon")

	provider := factory(slog.New(handler))

	pidPath := s.PidPath()
	sockPath := s.SocketPath()

	if err := store.WritePid(s.Fs(), pidPath, os.Getpid()); err != nil {
		return err
	}
	defer s.Fs().Remove(pidPath)

	s.Fs().Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	defer s.Fs().Remove(sockPath)

	srv := &server{
		provider: provider,
		log:      slog.New(handler).With("module", "grpc"),
	}
	grpcServer := grpc.NewServer()
	pb.RegisterDaemonServer(grpcServer, srv)

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		grpcServer.GracefulStop()
	}()

	log.Info("started", "pid", os.Getpid(), "socket", sockPath)
	return grpcServer.Serve(lis)
}
