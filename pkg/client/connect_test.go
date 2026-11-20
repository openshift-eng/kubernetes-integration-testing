package client

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemonstate"
)

// stubDaemon is a minimal daemon serving the RPCs Connect probes with.
type stubDaemon struct {
	pb.UnimplementedDaemonServer
}

func (stubDaemon) ListClusters(context.Context, *pb.ListClustersRequest) (*pb.ListClustersResponse, error) {
	return &pb.ListClustersResponse{}, nil
}

// fakeKitBinary writes an executable to use as KIT_BINARY, running the
// given shell script.
func fakeKitBinary(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-kit")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin
}

// serveStubDaemon starts serving a stub daemon on the socket path once the
// state directory exists. Creating that directory is the client's job, so
// the test must not do it first.
func serveStubDaemon(t *testing.T, paths daemonstate.Paths) <-chan error {
	t.Helper()
	srv := grpc.NewServer()
	pb.RegisterDaemonServer(srv, stubDaemon{})
	t.Cleanup(srv.Stop)

	errCh := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(paths.Root); err == nil {
				break
			}
			if time.Now().After(deadline) {
				errCh <- fmt.Errorf("state directory %s never appeared", paths.Root)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		lis, err := net.Listen("unix", paths.Socket())
		if err != nil {
			errCh <- fmt.Errorf("listening on %s: %w", paths.Socket(), err)
			return
		}
		errCh <- nil
		_ = srv.Serve(lis)
	}()
	return errCh
}

func testPaths(t *testing.T) daemonstate.Paths {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	paths, err := daemonstate.DefaultPaths()
	require.NoError(t, err)
	return paths
}

func TestConnectCleanHome(t *testing.T) {
	paths := testPaths(t)
	// A child that stays up while it is "starting": the socket is
	// provided by the test's gRPC server.
	t.Setenv("KIT_BINARY", fakeKitBinary(t, "#!/bin/sh\nsleep 5\n"))

	errCh := serveStubDaemon(t, paths)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { require.NoError(t, c.Close()) }()

	names, err := c.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, names)

	// The state directory was created by the client: the connection works
	// on a clean installation.
	assert.DirExists(t, paths.Root)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("stub daemon: %v", err)
		}
	default:
	}
}

func TestConnectHonorsContextWhileWaitingForSocket(t *testing.T) {
	testPaths(t)
	// A child that stays up but never creates a socket: Connect must give
	// up when the caller's context is done instead of waiting the full
	// default timeout.
	t.Setenv("KIT_BINARY", fakeKitBinary(t, "#!/bin/sh\nsleep 5\n"))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := Connect(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "waiting for daemon socket")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestConnectFailsFastWhenDaemonChildExits(t *testing.T) {
	testPaths(t)
	// The spawned child exits immediately with status 3 and no socket
	// ever appears: the wait must fail fast with the child's exit status
	// instead of burning the timeout.
	t.Setenv("KIT_BINARY", fakeKitBinary(t, "#!/bin/sh\nexit 3\n"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err := Connect(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon exited during startup")
	assert.Contains(t, err.Error(), "exit status 3")
	assert.Less(t, time.Since(start), 10*time.Second, "must fail fast on child exit")
}

func TestWaitForSocketToleratesLosingChild(t *testing.T) {
	// The spawned child exits, but the socket appeared anyway: another
	// concurrent starter's daemon won the instance lock. The wait must
	// succeed, not report the child exit.
	dir := t.TempDir()
	sock := filepath.Join(dir, "kit.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))

	child, err := startDaemonChild(fakeKitBinary(t, "#!/bin/sh\nexit 0\n"))
	require.NoError(t, err)

	require.NoError(t, waitForSocket(context.Background(), sock, child))
}

func TestDaemonStatus(t *testing.T) {
	paths := testPaths(t)
	require.NoError(t, os.MkdirAll(paths.Root, 0o700))

	st, err := Status()
	require.NoError(t, err)
	assert.False(t, st.Running)

	lock, err := daemonstate.AcquireInstanceLock(paths)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	st, err = Status()
	require.NoError(t, err)
	assert.True(t, st.Running)
}

func TestShutdownDaemonNotRunning(t *testing.T) {
	testPaths(t)

	err := ShutdownDaemon(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not running")
}

func TestShutdownDaemonAsksDaemonToStop(t *testing.T) {
	paths := testPaths(t)
	require.NoError(t, os.MkdirAll(paths.Root, 0o700))

	shutdownCh := make(chan struct{}, 1)
	srv := grpc.NewServer()
	pb.RegisterDaemonServer(srv, shutdownStub{shutdownCh: shutdownCh})
	lis, err := net.Listen("unix", paths.Socket())
	require.NoError(t, err)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	// A held instance lock is what tells ShutdownDaemon a daemon exists.
	lock, err := daemonstate.AcquireInstanceLock(paths)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	require.NoError(t, ShutdownDaemon(context.Background()))

	select {
	case <-shutdownCh:
	default:
		t.Fatal("shutdown RPC was not delivered to the daemon")
	}
}

type shutdownStub struct {
	pb.UnimplementedDaemonServer
	shutdownCh chan struct{}
}

func (s shutdownStub) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	s.shutdownCh <- struct{}{}
	return &pb.ShutdownResponse{}, nil
}
