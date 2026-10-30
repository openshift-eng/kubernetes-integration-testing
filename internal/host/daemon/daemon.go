package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"

	"github.com/spf13/afero"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/store"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
	"google.golang.org/grpc"
)

const defaultControllerImage = "registry.ci.openshift.org/ci/kubernetes-integration-testing:latest"

type ProviderFactory func(ctx context.Context, log *slog.Logger) cluster.Provider

type Daemon struct {
	paths   Paths
	fs      afero.Fs
	manager *cluster.ClusterManager
	log     *slog.Logger
}

func New(paths Paths, fs afero.Fs, manager *cluster.ClusterManager, log *slog.Logger) *Daemon {
	return &Daemon{paths: paths, fs: fs, manager: manager, log: log}
}

func (d *Daemon) Run(ctx context.Context) error {
	pidPath := d.paths.Pid()
	sockPath := d.paths.Socket()

	if err := WritePid(d.fs, pidPath, os.Getpid()); err != nil {
		return err
	}
	defer func() {
		if err := d.fs.Remove(pidPath); err != nil {
			d.log.Warn("removing pid file", "error", err)
		}
	}()

	if err := d.fs.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		d.log.Warn("removing stale socket", "error", err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		lis.Close()
		return fmt.Errorf("restricting socket permissions: %w", err)
	}
	defer func() {
		if err := d.fs.Remove(sockPath); err != nil {
			d.log.Warn("removing socket", "error", err)
		}
	}()

	srv := &server{
		clusterManager: d.manager,
		log:            d.log.With("module", "grpc"),
	}
	grpcServer := grpc.NewServer()
	pb.RegisterDaemonServer(grpcServer, srv)

	go func() {
		<-ctx.Done()
		d.log.Info("shutting down")
		grpcServer.GracefulStop()
	}()

	d.log.Info("started", "pid", os.Getpid(), "socket", sockPath)
	return grpcServer.Serve(lis)
}

func Start(ctx context.Context, paths Paths, appStore store.Store, factories map[string]ProviderFactory) error {
	logFile, err := os.OpenFile(paths.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			slog.Warn("closing log file", "error", err)
		}
	}()

	w := io.MultiWriter(logFile, os.Stderr)
	handler := slog.NewTextHandler(w, nil)
	slog.SetDefault(slog.New(handler))

	log := slog.New(handler).With("module", "daemon")

	providers := make(map[string]cluster.Provider, len(factories))
	for name, factory := range factories {
		providers[name] = factory(ctx, slog.New(handler))
	}

	controllerImage := os.Getenv("KIT_CONTROLLER_IMAGE")
	if controllerImage == "" {
		controllerImage = defaultControllerImage
	}
	launcher := cluster.NewPodmanControllerLauncher(
		podman.NewLocalClient(),
		controllerImage,
		appStore.ImageCacheDir(),
		appStore.CollectionsDir(),
		log.With("module", "launcher"),
	)

	manager := cluster.NewClusterManager(ctx, log, "kwok", providers, launcher)

	d := New(paths, afero.NewOsFs(), manager, log)
	return d.Run(ctx)
}
