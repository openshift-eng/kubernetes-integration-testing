package daemonstate

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPaths(t *testing.T) {
	p := NewPaths("/data")

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"Pid", p.Pid(), "/data/pid"},
		{"Socket", p.Socket(), "/data/kit.sock"},
		{"ClientLock", p.ClientLock(), "/data/kit.lock"},
		{"InstanceLock", p.InstanceLock(), "/data/instance.lock"},
		{"Log", p.Log(), "/data/daemon.log"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestDefaultPaths(t *testing.T) {
	p, err := DefaultPaths()
	require.NoError(t, err)
	assert.NotEmpty(t, p.Root)
}

func TestWriteAndReadPid(t *testing.T) {
	paths := NewPaths(t.TempDir())

	require.NoError(t, WritePid(paths, 12345))

	pid, err := ReadPid(paths)
	require.NoError(t, err)
	assert.Equal(t, 12345, pid)
}

func TestReadPidMissing(t *testing.T) {
	paths := NewPaths(t.TempDir())

	_, err := ReadPid(paths)
	require.Error(t, err)
}

func TestCleanupStale(t *testing.T) {
	paths := NewPaths(t.TempDir())
	require.NoError(t, os.MkdirAll(paths.Root, 0o700))

	for _, p := range []string{paths.Pid(), paths.Socket(), paths.ClientLock(), paths.InstanceLock()} {
		require.NoError(t, os.WriteFile(p, nil, 0o644), p)
	}

	CleanupStale(paths)

	// Stale runtime files are removed...
	assert.NoFileExists(t, paths.Pid())
	assert.NoFileExists(t, paths.Socket())
	// ...but the lock files must survive: replacing them would break the
	// locks by changing their inode.
	assert.FileExists(t, paths.ClientLock())
	assert.FileExists(t, paths.InstanceLock())
}

func TestInstanceLockLifecycle(t *testing.T) {
	paths := NewPaths(t.TempDir())

	assert.False(t, InstanceHeld(paths), "no daemon should hold the lock")

	lock, err := AcquireInstanceLock(paths)
	require.NoError(t, err)

	assert.True(t, InstanceHeld(paths), "the acquired lock must be visible as held")

	_, err = AcquireInstanceLock(paths)
	require.Error(t, err, "a second daemon must not be able to start")
	assert.Contains(t, err.Error(), "already running")

	require.NoError(t, lock.Release())
	assert.False(t, InstanceHeld(paths), "the lock must be free after release")
}

func TestInstanceLockReleasedOnClose(t *testing.T) {
	paths := NewPaths(t.TempDir())

	// A daemon dying without calling Release: the closed file (like an
	// exited process) drops the lock.
	lock, err := AcquireInstanceLock(paths)
	require.NoError(t, err)
	require.NoError(t, lock.f.Close())
	lock.f = nil

	assert.False(t, InstanceHeld(paths))
}
