// Package daemonstate owns the on-disk state shared by the kit daemon
// and its clients: the state directory layout, the pid file, and the
// locks that guarantee a single daemon instance.
//
// The instance lock is the authority on daemon liveness: the daemon holds
// it for its entire lifetime and the kernel releases it when the process
// dies, so it can never go stale. The pid file is informational only.
package daemonstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// Paths resolves the locations of the daemon state files within a root
// directory.
type Paths struct {
	Root string
}

// NewPaths returns Paths rooted at root.
func NewPaths(root string) Paths {
	return Paths{Root: root}
}

// DefaultPaths returns the default daemon paths, rooted at
// ~/.local/share/openshift-kit.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return NewPaths(filepath.Join(home, ".local", "share", "openshift-kit")), nil
}

// Pid is the path of the daemon pid file.
func (p Paths) Pid() string { return filepath.Join(p.Root, "pid") }

// Socket is the path of the daemon gRPC unix socket.
func (p Paths) Socket() string { return filepath.Join(p.Root, "kit.sock") }

// ClientLock is the path of the lock that serializes clients deciding
// whether to spawn a daemon. It is never removed: replacing the file
// would break the lock by changing its inode.
func (p Paths) ClientLock() string { return filepath.Join(p.Root, "kit.lock") }

// InstanceLock is the path of the lock held by the running daemon for
// its entire lifetime. It is never removed: replacing the file would
// break the lock by changing its inode.
func (p Paths) InstanceLock() string { return filepath.Join(p.Root, "instance.lock") }

// Log is the path of the daemon log file.
func (p Paths) Log() string { return filepath.Join(p.Root, "daemon.log") }

// WritePid records the daemon pid.
func WritePid(paths Paths, pid int) error {
	return os.WriteFile(paths.Pid(), []byte(strconv.Itoa(pid)), 0o644)
}

// ReadPid reads the daemon pid.
//
// The pid is informational: a live pid does not prove the process is the
// kit daemon, because pids are reused. Liveness is answered by the
// instance lock, and nothing may signal a process based on this file.
func ReadPid(paths Paths) (int, error) {
	data, err := os.ReadFile(paths.Pid())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

// CleanupStale removes stale pid and socket files. It never sends
// signals and never removes the lock files.
func CleanupStale(paths Paths) {
	_ = os.Remove(paths.Pid())
	_ = os.Remove(paths.Socket())
}

// InstanceLock is the daemon singleton lock. The daemon acquires it
// before initializing and holds it until it exits; the kernel releases
// it if the process dies, so a held lock always means a live daemon.
type InstanceLock struct {
	f *os.File
}

// AcquireInstanceLock takes the daemon instance lock. It fails if a
// daemon is already running. The caller must keep the returned lock for
// the daemon's lifetime; Release is only called on graceful exit.
func AcquireInstanceLock(paths Paths) (*InstanceLock, error) {
	if err := os.MkdirAll(paths.Root, 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}

	f, err := os.OpenFile(paths.InstanceLock(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening instance lock: %w", err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("daemon already running")
		}
		return nil, fmt.Errorf("acquiring instance lock: %w", err)
	}

	return &InstanceLock{f: f}, nil
}

// Release drops the instance lock. It is a no-op after process exit: the
// kernel has already released it.
func (l *InstanceLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// InstanceHeld reports whether a daemon currently holds the instance
// lock. A held lock is proof that a daemon is running; a free lock is
// proof that none is, regardless of what the pid file says.
func InstanceHeld(paths Paths) bool {
	f, err := os.OpenFile(paths.InstanceLock(), os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	// Probe without waiting: if we get the lock, it was free.
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return false
	}
	return errors.Is(err, unix.EWOULDBLOCK)
}
