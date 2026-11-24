package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemonstate"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/imageref"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/store"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
	"github.com/openshift-eng/kubernetes-integration-testing/pkg/version"
)

type ProviderFactory func(ctx context.Context, log *slog.Logger) cluster.Provider

type Daemon struct {
	paths    daemonstate.Paths
	manager  cluster.Manager
	log      *slog.Logger
	shutdown func()
}

func New(paths daemonstate.Paths, manager cluster.Manager, log *slog.Logger, shutdown func()) *Daemon {
	return &Daemon{paths: paths, manager: manager, log: log, shutdown: shutdown}
}

func (d *Daemon) Run(ctx context.Context) error {
	pidPath := d.paths.Pid()
	sockPath := d.paths.Socket()

	if err := daemonstate.WritePid(d.paths, os.Getpid()); err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(pidPath); err != nil {
			d.log.Warn("removing pid file", "error", err)
		}
	}()

	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		d.log.Warn("removing stale socket", "error", err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return errors.Join(fmt.Errorf("restricting socket permissions: %w", err), lis.Close())
	}
	defer func() {
		if err := os.Remove(sockPath); err != nil {
			d.log.Warn("removing socket", "error", err)
		}
	}()

	d.recoverClusters(ctx)

	srv := &server{
		clusterManager: d.manager,
		shutdown:       d.shutdown,
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

const recoverTimeout = 3 * time.Minute

func (d *Daemon) recoverClusters(ctx context.Context) {
	d.log.Info("recovering existing clusters")
	recoverCtx, recoverCancel := context.WithTimeout(ctx, recoverTimeout)
	defer recoverCancel()
	d.manager.Recover(recoverCtx)
}

func controllerImageRef() (imageref.Requested, error) {
	raw := os.Getenv("KIT_CONTROLLER_IMAGE")
	if raw == "" {
		raw = version.DefaultControllerImage
	}
	return imageref.ParseRequested(raw)
}

func Start(ctx context.Context, paths daemonstate.Paths, appStore store.Store, podmanClient podman.Client, factories map[string]ProviderFactory) error {
	if err := os.MkdirAll(paths.Root, 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	logFile, err := os.OpenFile(paths.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := logFile.Chmod(0o600); err != nil {
		return errors.Join(err, logFile.Close())
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

	// The instance lock is acquired before anything else initializes and
	// held until the daemon exits. It is what guarantees a single daemon:
	// a second one started concurrently fails here instead of racing the
	// first one for the pid file and the socket. The kernel releases the
	// lock if the process dies.
	instanceLock, err := daemonstate.AcquireInstanceLock(paths)
	if err != nil {
		return err
	}
	defer func() {
		if err := instanceLock.Release(); err != nil {
			log.Warn("releasing instance lock", "error", err)
		}
	}()

	controllerImage, err := controllerImageRef()
	if err != nil {
		return fmt.Errorf("invalid controller image: %w", err)
	}

	providers := make(map[string]cluster.Provider, len(factories))
	for name, factory := range factories {
		providers[name] = factory(ctx, slog.New(handler))
	}

	launcher := cluster.NewPodmanControllerLauncher(
		podmanClient,
		appStore.ImageCacheDir(),
		appStore.CollectionsDir(),
		log.With("module", "launcher"),
	)

	resolver := imageref.NewPodmanResolver(podmanClient, controllerImage, log)
	manager := cluster.NewClusterManager(ctx, log, "kwok", providers, resolver, launcher)

	// runCtx is cancelled both by the caller's context (signals) and by
	// the Shutdown RPC.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	d := New(paths, manager, log, cancel)
	return d.Run(runCtx)
}
