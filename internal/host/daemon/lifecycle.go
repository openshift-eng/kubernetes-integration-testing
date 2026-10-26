package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/spf13/afero"
)

type Paths struct {
	Root string
}

func NewPaths(root string) Paths {
	return Paths{Root: root}
}

func (p Paths) Pid() string    { return filepath.Join(p.Root, "pid") }
func (p Paths) Socket() string { return filepath.Join(p.Root, "kit.sock") }
func (p Paths) Lock() string   { return filepath.Join(p.Root, "kit.lock") }
func (p Paths) Log() string    { return filepath.Join(p.Root, "daemon.log") }

func WritePid(fs afero.Fs, path string, pid int) error {
	return afero.WriteFile(fs, path, []byte(strconv.Itoa(pid)), 0o644)
}

func ReadPid(fs afero.Fs, path string) (int, error) {
	data, err := afero.ReadFile(fs, path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

func IsRunning(fs afero.Fs, pidPath, sockPath string) (int, bool) {
	pid, err := ReadPid(fs, pidPath)
	if err != nil {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		Cleanup(fs, pidPath, sockPath)
		return 0, false
	}
	exists, _ := afero.Exists(fs, sockPath)
	if !exists {
		Cleanup(fs, pidPath, sockPath)
		return 0, false
	}
	return pid, true
}

func Stop(fs afero.Fs, pidPath, sockPath string) error {
	pid, running := IsRunning(fs, pidPath, sockPath)
	if !running {
		return fmt.Errorf("not running")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}
	return proc.Signal(syscall.SIGTERM)
}

// Cleanup is best-effort: it kills the process and removes stale files.
func Cleanup(fs afero.Fs, pidPath, sockPath string) {
	if pid, err := ReadPid(fs, pidPath); err == nil {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
	_ = fs.Remove(pidPath)
	_ = fs.Remove(sockPath)
}
