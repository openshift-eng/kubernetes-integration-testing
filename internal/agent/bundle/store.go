package bundle

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/spf13/afero"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func IsValidDigest(s string) bool {
	return digestPattern.MatchString(s)
}

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

	bundlePath, err := s.bundlePath(b.Digest)
	if err != nil {
		return err
	}
	if exists, _ := afero.Exists(s.fs, bundlePath); !exists {
		tmpPath := bundlePath + ".tmp"
		if err := afero.WriteFile(s.fs, tmpPath, b.data, 0644); err != nil {
			return fmt.Errorf("writing bundle %s: %w", b.Digest, err)
		}
		if err := s.fs.Rename(tmpPath, bundlePath); err != nil {
			_ = s.fs.Remove(tmpPath)
			return fmt.Errorf("renaming bundle %s: %w", b.Digest, err)
		}
	}

	s.agents[agentName] = b.Digest
	return s.writeIndex()
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

func (s *BundleStore) activeDigests() map[string]struct{} {
	active := make(map[string]struct{}, len(s.agents))
	for _, d := range s.agents {
		active[d] = struct{}{}
	}
	return active
}

func (s *BundleStore) listBlobs() ([]string, error) {
	entries, err := afero.ReadDir(s.fs, s.dir)
	if err != nil {
		return nil, fmt.Errorf("listing bundles directory: %w", err)
	}
	const suffix = ".tar.gz"
	var digests []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		d := name[:len(name)-len(suffix)]
		if digestPattern.MatchString(d) {
			digests = append(digests, d)
		}
	}
	return digests, nil
}

func (s *BundleStore) purgeStaleAgents(activeAgents map[string]struct{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dirty := false
	for name := range s.agents {
		if _, ok := activeAgents[name]; !ok {
			delete(s.agents, name)
			dirty = true
			s.log.Info("bundle GC: purged stale agent mapping", "agent", name)
		}
	}
	if dirty {
		return s.writeIndex()
	}
	return nil
}

func (s *BundleStore) snapshotForGC() (blobs []string, active map[string]struct{}, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	blobs, err = s.listBlobs()
	if err != nil {
		return nil, nil, err
	}
	return blobs, s.activeDigests(), nil
}

// deleteIfUnreferenced removes a blob only if it is not in the internal
// agent→digest mapping and not in the externally protected set. The check
// and the filesystem delete happen under the same lock, closing the TOCTOU
// gap between listing active digests and removing blobs.
func (s *BundleStore) deleteIfUnreferenced(digest string, protected map[string]struct{}) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := protected[digest]; ok {
		return false, nil
	}

	for _, activeDigest := range s.agents {
		if activeDigest == digest {
			return false, nil
		}
	}

	path, err := s.bundlePath(digest)
	if err != nil {
		return false, err
	}

	if err := s.fs.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("removing bundle blob %s: %w", digest, err)
	}

	return true, nil
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
