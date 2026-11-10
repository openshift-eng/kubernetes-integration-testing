package bundle

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/spf13/afero"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type BundleStore struct {
	mu     sync.RWMutex
	fs     afero.Fs
	dir    string
	agents map[string]string
	log    *slog.Logger
}

type storeIndex struct {
	Agents map[string]string `json:"agents"`
}

func NewBundleStore(fs afero.Fs, dir string, log *slog.Logger) (*BundleStore, error) {
	if err := fs.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating bundles directory: %w", err)
	}

	s := &BundleStore{
		fs:     fs,
		dir:    dir,
		agents: make(map[string]string),
		log:    log,
	}

	if err := s.loadIndex(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *BundleStore) Put(agentName string, b *Bundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldDigest := s.agents[agentName]

	bundlePath, err := s.bundlePath(b.Digest)
	if err != nil {
		return err
	}
	if exists, _ := afero.Exists(s.fs, bundlePath); !exists {
		if err := afero.WriteFile(s.fs, bundlePath, b.data, 0644); err != nil {
			return fmt.Errorf("writing bundle %s: %w", b.Digest, err)
		}
	}

	s.agents[agentName] = b.Digest

	if oldDigest != "" && oldDigest != b.Digest && !s.isReferenced(oldDigest) {
		oldPath, err := s.bundlePath(oldDigest)
		if err != nil {
			return err
		}
		if err := s.fs.Remove(oldPath); err != nil {
			return fmt.Errorf("removing old bundle %s: %w", oldDigest, err)
		}
	}

	return s.writeIndex()
}

func (s *BundleStore) Resolve(agentName string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	digest, ok := s.agents[agentName]
	return digest, ok
}

func (s *BundleStore) Open(digest string) (io.ReadCloser, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path, err := s.bundlePath(digest)
	if err != nil {
		return nil, 0, err
	}
	fi, err := s.fs.Stat(path)
	if err != nil {
		return nil, 0, fmt.Errorf("stat bundle %s: %w", digest, err)
	}
	f, err := s.fs.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("opening bundle %s: %w", digest, err)
	}
	return f, fi.Size(), nil
}

func (s *BundleStore) Remove(agentName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	digest, ok := s.agents[agentName]
	if !ok {
		return nil
	}

	delete(s.agents, agentName)

	if !s.isReferenced(digest) {
		path, err := s.bundlePath(digest)
		if err != nil {
			return err
		}
		if err := s.fs.Remove(path); err != nil {
			return fmt.Errorf("removing bundle %s: %w", digest, err)
		}
	}

	return s.writeIndex()
}

func (s *BundleStore) isReferenced(digest string) bool {
	for _, d := range s.agents {
		if d == digest {
			return true
		}
	}
	return false
}

func (s *BundleStore) bundlePath(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid digest %q", digest)
	}
	return filepath.Join(s.dir, digest+".tar.gz"), nil
}

func (s *BundleStore) indexPath() string {
	return filepath.Join(s.dir, "index.json")
}

func (s *BundleStore) loadIndex() error {
	data, err := afero.ReadFile(s.fs, s.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading index: %w", err)
	}

	var idx storeIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		s.log.Warn("corrupt bundle index, starting fresh", "error", err)
		return nil
	}

	for name, digest := range idx.Agents {
		path, err := s.bundlePath(digest)
		if err != nil {
			s.log.Warn("skipping index entry with invalid digest", "agent", name, "digest", digest)
			continue
		}
		if exists, _ := afero.Exists(s.fs, path); exists {
			s.agents[name] = digest
		}
	}

	return nil
}

func (s *BundleStore) writeIndex() error {
	idx := storeIndex{Agents: s.agents}
	data, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("marshaling index: %w", err)
	}
	tmpPath := s.indexPath() + ".tmp"
	if err := afero.WriteFile(s.fs, tmpPath, data, 0644); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}
	return s.fs.Rename(tmpPath, s.indexPath())
}
