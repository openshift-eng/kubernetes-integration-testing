package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/proto"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/store"
	"google.golang.org/grpc"
)

type ProviderFactory func(log *slog.Logger) cluster.Provider

func Run(ctx context.Context, s store.Store, factories map[string]ProviderFactory, foreground bool) error {
	logFile, err := os.OpenFile(s.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			slog.Warn("closing log file", "error", err)
		}
	}()

	var w io.Writer = logFile
	if foreground {
		w = io.MultiWriter(logFile, os.Stderr)
	}
	handler := slog.NewTextHandler(w, nil)
	slog.SetDefault(slog.New(handler))

	log := slog.New(handler).With("module", "daemon")

	providers := make(map[string]cluster.Provider, len(factories))
	for name, factory := range factories {
		providers[name] = factory(slog.New(handler))
	}
	registry := cluster.NewRegistry("kwok", providers)

	pidPath := s.PidPath()
	sockPath := s.SocketPath()

	if err := store.WritePid(s.Fs(), pidPath, os.Getpid()); err != nil {
		return err
	}
	defer func() {
		if err := s.Fs().Remove(pidPath); err != nil {
			log.Warn("removing pid file", "error", err)
		}
	}()

	if err := s.Fs().Remove(sockPath); err != nil && !os.IsNotExist(err) {
		log.Warn("removing stale socket", "error", err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := s.Fs().Remove(sockPath); err != nil {
			log.Warn("removing socket", "error", err)
		}
	}()

	srv := &server{
		registry: registry,
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
