package store

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

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
	CollectionsDir() string
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
		filepath.Join(root, "collections"),
		filepath.Join(root, "images"),
	}
	for _, d := range dirs {
		if err := fs.MkdirAll(d, 0o700); err != nil {
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

func (s *store) CollectionsDir() string {
	return filepath.Join(s.root, "collections")
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
	if err := s.fs.MkdirAll(dir, 0o700); err != nil {
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
