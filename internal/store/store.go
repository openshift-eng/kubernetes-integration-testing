package store

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/spf13/afero"
)

type FetchFunc func(dest string) error

type Store interface {
	Fs() afero.Fs
	Register(id string, fetch FetchFunc)
	Has(id string) bool
	Get(id string) (string, error)
	ClusterDir(name string) (string, error)
	RemoveClusterDir(name string) error
	ImageCacheDir() string
	PidPath() string
	SocketPath() string
	LogPath() string
}

type store struct {
	fs   afero.Fs
	root string

	mu       sync.Mutex
	registry map[string]FetchFunc
}

func New() (Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newStore(afero.NewOsFs(), filepath.Join(home, ".local", "share", "openshift-kit"))
}

func NewWithRoot(fs afero.Fs, root string) (Store, error) {
	return newStore(fs, root)
}

func newStore(fs afero.Fs, root string) (Store, error) {
	dirs := []string{
		root,
		filepath.Join(root, "cache"),
		filepath.Join(root, "clusters"),
		filepath.Join(root, "images"),
	}
	for _, d := range dirs {
		if err := fs.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &store{
		fs:       fs,
		root:     root,
		registry: make(map[string]FetchFunc),
	}, nil
}

func (s *store) Fs() afero.Fs {
	return s.fs
}

func (s *store) ImageCacheDir() string {
	return filepath.Join(s.root, "images")
}

func (s *store) PidPath() string {
	return filepath.Join(s.root, "pid")
}

func (s *store) SocketPath() string {
	return filepath.Join(s.root, "kit.sock")
}

func (s *store) LogPath() string {
	return filepath.Join(s.root, "daemon.log")
}

func (s *store) Register(id string, fetch FetchFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[id] = fetch
}

func (s *store) Has(id string) bool {
	path := filepath.Join(s.root, "cache", id)
	exists, _ := afero.Exists(s.fs, path)
	return exists
}

func (s *store) ClusterDir(name string) (string, error) {
	dir := filepath.Join(s.root, "clusters", name)
	if err := s.fs.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *store) RemoveClusterDir(name string) error {
	return s.fs.RemoveAll(filepath.Join(s.root, "clusters", name))
}

func (s *store) Get(id string) (string, error) {
	path := filepath.Join(s.root, "cache", id)

	exists, err := afero.Exists(s.fs, path)
	if err != nil {
		return "", err
	}
	if exists {
		return path, nil
	}

	s.mu.Lock()
	fetch, ok := s.registry[id]
	s.mu.Unlock()

	if !ok {
		return "", fmt.Errorf("unknown content %q: not registered", id)
	}

	if err := fetch(path); err != nil {
		return "", fmt.Errorf("fetching %q: %w", id, err)
	}
	return path, nil
}

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

//nolint:errcheck
func IsRunning(pidPath, sockPath string) (int, bool) {
	pid, err := ReadPid(afero.NewOsFs(), pidPath)
	if err != nil {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		Cleanup(pidPath, sockPath)
		return 0, false
	}
	exists, _ := afero.Exists(afero.NewOsFs(), sockPath)
	if !exists {
		Cleanup(pidPath, sockPath)
		return 0, false
	}
	return pid, true
}

func Stop(pidPath, sockPath string) error {
	pid, running := IsRunning(pidPath, sockPath)
	if !running {
		return fmt.Errorf("not running")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}
	return proc.Signal(syscall.SIGTERM)
}

//nolint:errcheck
func Cleanup(pidPath, sockPath string) {
	osFs := afero.NewOsFs()
	if pid, err := ReadPid(osFs, pidPath); err == nil {
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Signal(syscall.SIGTERM)
		}
	}
	osFs.Remove(pidPath)
	osFs.Remove(sockPath)
}

func DownloadBinary(url, dest string) (err error) {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, resp.Body.Close())
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), "download-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmpPath))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmpPath))
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return errors.Join(err, os.Remove(tmpPath))
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		return errors.Join(err, os.Remove(tmpPath))
	}

	return nil
}
