// Package client provides the public API for talking to the kit daemon.
// It exposes the cluster operations that the kit CLI itself uses, so
// external programs (for example integration test suites) can drive
// clusters the same way the CLI does.
//
// The daemon lifecycle details (state paths, pid file, locks) live in
// the internal daemonstate package; this package only surfaces
// intent-level operations on them.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/openshift-eng/kubernetes-integration-testing/api/grpc"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemonstate"
)

// Client talks to the kit daemon, starting it if it is not already
// running. Create one per command or test; the daemon itself is shared.
type Client struct {
	pb   pb.DaemonClient
	conn *grpc.ClientConn

	// binary is the kit executable used to spawn a missing daemon.
	binary string
}

// Option configures a Client.
type Option func(*Client)

// WithBinary sets the kit executable used to spawn a missing daemon.
// Defaults to $KIT_BINARY, then the current executable. Programs that are
// not the kit binary itself (for example test binaries using this
// package) must set this or KIT_BINARY.
func WithBinary(path string) Option {
	return func(c *Client) {
		c.binary = path
	}
}

// daemonBinary returns the kit executable to spawn when the daemon is not
// running: $KIT_BINARY if set, otherwise the current executable.
func daemonBinary() string {
	if bin := os.Getenv("KIT_BINARY"); bin != "" {
		return bin
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return ""
}

// Connect dials the kit daemon at its default location, starting it in
// the background if needed, and returns a Client for it.
//
// Connect only returns once the daemon answers RPCs: the daemon listens
// on its socket before it serves traffic (it recovers existing clusters
// in between), so Connect probes it with a cheap read-only call. ctx
// bounds every wait Connect performs; callers wanting a bound must set a
// deadline on it.
func Connect(ctx context.Context, opts ...Option) (*Client, error) {
	paths, err := daemonstate.DefaultPaths()
	if err != nil {
		return nil, err
	}
	c := &Client{binary: daemonBinary()}
	for _, opt := range opts {
		opt(c)
	}

	if err := c.ensureDaemon(ctx, paths); err != nil {
		return nil, err
	}

	conn, err := dial(paths.Socket())
	if err != nil {
		return nil, err
	}
	c.conn = conn
	c.pb = pb.NewDaemonClient(conn)

	if _, err := c.pb.ListClusters(ctx, &pb.ListClustersRequest{}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("daemon not ready: %w", err)
	}
	return c, nil
}

// Close releases the connection to the daemon.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// DaemonStatus reports the state of the daemon. Running is based on the
// instance lock, so it is never fooled by a stale pid file or pid reuse.
// Pid is best-effort, for display only.
type DaemonStatus struct {
	Running bool
	Pid     int
}

// Status reports whether the daemon is running.
func Status() (DaemonStatus, error) {
	paths, err := daemonstate.DefaultPaths()
	if err != nil {
		return DaemonStatus{}, err
	}
	st := DaemonStatus{Running: daemonstate.InstanceHeld(paths)}
	if pid, err := daemonstate.ReadPid(paths); err == nil {
		st.Pid = pid
	}
	return st, nil
}

// ShutdownDaemon asks the running daemon to terminate gracefully. Asking
// the daemon over its own protocol doubles as the identity check: only
// the real daemon answers, so no signal is ever sent to a pid read from
// a file. It never starts a daemon.
func ShutdownDaemon(ctx context.Context) error {
	paths, err := daemonstate.DefaultPaths()
	if err != nil {
		return err
	}

	if !daemonstate.InstanceHeld(paths) {
		daemonstate.CleanupStale(paths)
		return fmt.Errorf("not running")
	}

	conn, err := dial(paths.Socket())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if _, err := pb.NewDaemonClient(conn).Shutdown(ctx, &pb.ShutdownRequest{}); err != nil {
		if status.Code(err) == codes.Unavailable {
			return fmt.Errorf("daemon is starting or unreachable")
		}
		return err
	}
	return nil
}

// DaemonLogPath returns the path of the daemon log file.
func DaemonLogPath() (string, error) {
	paths, err := daemonstate.DefaultPaths()
	if err != nil {
		return "", err
	}
	return paths.Log(), nil
}

// StartDaemonBackground starts a new daemon process from the given kit
// executable, detached from the current session. It reports an error if
// the process exits within startupGrace, so callers do not report a
// successful start for a daemon that died immediately (for example
// because another daemon already holds the instance lock).
func StartDaemonBackground(binary string, startupGrace time.Duration) (int, error) {
	child, err := startDaemonChild(binary)
	if err != nil {
		return 0, err
	}

	select {
	case err := <-child.exited:
		if err != nil {
			return 0, fmt.Errorf("daemon exited during startup: %w", err)
		}
		return 0, fmt.Errorf("daemon exited during startup")
	case <-time.After(startupGrace):
	}

	return child.cmd.Process.Pid, nil
}

// daemonChild is a spawned daemon process with a background Wait that
// both reaps it when it exits and reports early failures to whoever is
// waiting for the daemon to come up.
type daemonChild struct {
	cmd    *exec.Cmd
	exited chan error // receives the result of Wait exactly once
}

func startDaemonChild(binary string) (*daemonChild, error) {
	cmd := exec.Command(binary, "start", "--foreground")
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting background process: %w", err)
	}

	child := &daemonChild{cmd: cmd, exited: make(chan error, 1)}
	go func() {
		child.exited <- child.cmd.Wait()
	}()
	return child, nil
}

// dial connects to the daemon gRPC unix socket.
func dial(sockPath string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(
		"passthrough:///unix",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return net.DialTimeout("unix", sockPath, 3*time.Second)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to daemon: %w", err)
	}
	return conn, nil
}

// ensureDaemon spawns the daemon if it is not running and waits for its
// socket. The decision to spawn is taken exactly once, under the client
// lock that serializes concurrent starters. The instance lock is the
// authority on whether a daemon is running, so a stale pid file cannot
// suppress a needed spawn and pid reuse cannot cause a double spawn:
// even if two daemons were started, the second would exit immediately at
// the instance lock. All waits honor ctx.
func (c *Client) ensureDaemon(ctx context.Context, paths daemonstate.Paths) error {
	if err := os.MkdirAll(paths.Root, 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	f, err := os.OpenFile(paths.ClientLock(), os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening lock file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := waitLock(ctx, int(f.Fd())); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	var child *daemonChild
	if !daemonstate.InstanceHeld(paths) {
		daemonstate.CleanupStale(paths)
		if c.binary == "" {
			return fmt.Errorf("no kit binary available to start the daemon: set KIT_BINARY or pass WithBinary")
		}
		if child, err = startDaemonChild(c.binary); err != nil {
			return fmt.Errorf("could not start daemon: %w", err)
		}
	}

	return waitForSocket(ctx, paths.Socket(), child)
}

// waitLock acquires the client startup lock, waiting until it is
// available or ctx is done.
func waitLock(ctx context.Context, fd int) error {
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("acquiring lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquiring daemon lock: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// waitForSocket waits for the daemon socket to appear, bounded both by
// ctx and by an overall timeout. When this process spawned the daemon,
// an early exit of the child fails the wait immediately instead of
// burning the timeout: the child's Wait result is delivered on exited.
// The socket gets one final check first, because the exited child may be
// a loser of the instance lock while another starter's daemon won it.
func waitForSocket(ctx context.Context, sockPath string, child *daemonChild) error {
	var exited <-chan error
	if child != nil {
		exited = child.exited
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(60 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for daemon socket: %w", ctx.Err())
		case <-timeout:
			return fmt.Errorf("daemon started but socket not ready")
		case err := <-exited:
			if _, statErr := os.Stat(sockPath); statErr == nil {
				return nil
			}
			if err != nil {
				return fmt.Errorf("daemon exited during startup: %w", err)
			}
			return fmt.Errorf("daemon exited during startup")
		case <-ticker.C:
		}
	}
}
