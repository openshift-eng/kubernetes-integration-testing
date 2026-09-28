package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Image struct {
	Ref    string
	Digest v1.Hash
	raw    v1.Image
}

type Matcher struct {
	Pattern *regexp.Regexp
}

type Registry struct {
	mu       sync.Mutex
	cacheDir string
	keychain authn.Keychain
	log      *slog.Logger
}

func NewRegistry(cacheDir string, keychain authn.Keychain, log *slog.Logger) *Registry {
	return &Registry{
		cacheDir: cacheDir,
		keychain: keychain,
		log:      log,
	}
}

func KeychainFromFile(path string) (authn.Keychain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading pull secret: %w", err)
	}

	var cfg struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing pull secret: %w", err)
	}

	return &staticKeychain{auths: cfg.Auths}, nil
}

func (r *Registry) Pull(ref string) (Image, error) {
	return r.pull(ref)
}

func (r *Registry) pull(ref string) (Image, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	parsed, err := name.ParseReference(ref)
	if err != nil {
		return Image{}, fmt.Errorf("parsing reference %q: %w", ref, err)
	}

	r.log.Info("resolving image", "ref", ref)

	desc, err := remote.Get(parsed, remote.WithAuthFromKeychain(r.keychain))
	if err != nil {
		return Image{}, fmt.Errorf("pulling %q: %w", ref, err)
	}

	digest := desc.Digest

	if img, err := r.loadCached(digest); err == nil {
		r.log.Info("image already cached", "ref", ref, "digest", digest)
		return Image{Ref: ref, Digest: digest, raw: img}, nil
	}

	img, err := desc.Image()
	if err != nil {
		return Image{}, fmt.Errorf("reading image %q: %w", ref, err)
	}

	lp, err := r.ensureLayout(digest)
	if err != nil {
		return Image{}, fmt.Errorf("preparing cache: %w", err)
	}

	if err := lp.WriteImage(img); err != nil {
		return Image{}, fmt.Errorf("caching image: %w", err)
	}

	r.log.Info("pulled and cached image", "ref", ref, "digest", digest)
	return Image{Ref: ref, Digest: digest, raw: img}, nil
}

func (r *Registry) Extract(ref string, matchers []Matcher) (map[string]string, error) {
	img, err := r.pull(ref)
	if err != nil {
		return nil, err
	}

	filesDir := filepath.Join(r.digestDir(img.Digest), "files")

	results := make(map[string]string)
	var pending []Matcher
	for _, m := range matchers {
		cached := r.findCached(filesDir, m.Pattern)
		if cached != "" {
			results[m.Pattern.String()] = cached
			continue
		}
		pending = append(pending, m)
	}

	if len(pending) == 0 {
		r.log.Info("all files already extracted", "ref", ref)
		return results, nil
	}

	layers, err := img.raw.Layers()
	if err != nil {
		return nil, fmt.Errorf("reading layers: %w", err)
	}

	r.log.Info("extracting files", "ref", ref, "pending", len(pending), "cached", len(results), "layers", len(layers))

	remaining := len(pending)

	for i := len(layers) - 1; i >= 0 && remaining > 0; i-- {
		rc, err := layers[i].Compressed()
		if err != nil {
			return results, fmt.Errorf("reading layer %d: %w", i, err)
		}

		found, err := extractFromLayer(rc, filesDir, pending, results)
		rc.Close()
		if err != nil {
			return results, err
		}
		remaining -= found
	}

	if remaining > 0 {
		var missing []string
		for _, m := range pending {
			if _, ok := results[m.Pattern.String()]; !ok {
				missing = append(missing, m.Pattern.String())
			}
		}
		return results, fmt.Errorf("files not found in image: %s", strings.Join(missing, ", "))
	}

	return results, nil
}

// ExtractDir extracts all files under a directory prefix from the image.
// Returns a map of relative path -> local path for each extracted file.
func (r *Registry) ExtractDir(ref string, dirPrefix string) (map[string]string, error) {
	img, err := r.pull(ref)
	if err != nil {
		return nil, err
	}

	filesDir := filepath.Join(r.digestDir(img.Digest), "files")
	prefix := strings.TrimPrefix(dirPrefix, "/")
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	cachedDir := filepath.Join(filesDir, prefix)
	if entries, err := os.ReadDir(cachedDir); err == nil && len(entries) > 0 {
		r.log.Info("using cached directory", "ref", ref, "dir", dirPrefix, "files", len(entries))
		results := make(map[string]string, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				results[e.Name()] = filepath.Join(cachedDir, e.Name())
			}
		}
		return results, nil
	}

	layers, err := img.raw.Layers()
	if err != nil {
		return nil, fmt.Errorf("reading layers: %w", err)
	}

	r.log.Info("extracting directory", "ref", ref, "dir", dirPrefix, "layers", len(layers))

	results := make(map[string]string)

	for i := len(layers) - 1; i >= 0; i-- {
		rc, err := layers[i].Compressed()
		if err != nil {
			return results, fmt.Errorf("reading layer %d: %w", i, err)
		}

		if err := extractDirFromLayer(rc, filesDir, prefix, results); err != nil {
			rc.Close()
			return results, err
		}
		rc.Close()
	}

	r.log.Info("extracted directory", "ref", ref, "dir", dirPrefix, "files", len(results))
	return results, nil
}

func (r *Registry) findCached(filesDir string, pattern *regexp.Regexp) string {
	var found string
	_ = filepath.WalkDir(filesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return err
		}
		rel, _ := filepath.Rel(filesDir, path)
		if pattern.MatchString(rel) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func (r *Registry) digestDir(digest v1.Hash) string {
	return filepath.Join(r.cacheDir, digest.Algorithm, digest.Hex)
}

func (r *Registry) loadCached(digest v1.Hash) (v1.Image, error) {
	dir := r.digestDir(digest)
	lp, err := layout.FromPath(dir)
	if err != nil {
		return nil, err
	}
	return lp.Image(digest)
}

func (r *Registry) ensureLayout(digest v1.Hash) (layout.Path, error) {
	dir := r.digestDir(digest)
	if lp, err := layout.FromPath(dir); err == nil {
		return lp, nil
	}
	return layout.Write(dir, emptyIndex())
}

func extractFromLayer(rc io.ReadCloser, filesDir string, matchers []Matcher, results map[string]string) (int, error) {
	gr, err := gzip.NewReader(rc)
	if err != nil {
		return 0, fmt.Errorf("decompressing layer: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	found := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return found, nil
		}
		if err != nil {
			return found, fmt.Errorf("reading tar entry: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		clean := filepath.Clean(strings.TrimPrefix(hdr.Name, "./"))

		for i := range matchers {
			key := matchers[i].Pattern.String()
			if _, already := results[key]; already {
				continue
			}
			if !matchers[i].Pattern.MatchString(clean) {
				continue
			}

			dest := filepath.Join(filesDir, clean)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return found, err
			}

			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode()|0o644)
			if err != nil {
				return found, err
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return found, err
			}
			f.Close()

			results[key] = dest
			found++
			break
		}
	}
}

func extractDirFromLayer(rc io.ReadCloser, filesDir, prefix string, results map[string]string) error {
	gr, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("decompressing layer: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		clean := filepath.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if !strings.HasPrefix(clean, prefix) {
			continue
		}

		rel := strings.TrimPrefix(clean, prefix)
		if _, already := results[rel]; already {
			continue
		}

		dest := filepath.Join(filesDir, clean)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}

		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode()|0o644)
		if err != nil {
			return err
		}

		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()

		results[rel] = dest
	}
}

type staticKeychain struct {
	auths map[string]authn.AuthConfig
}

func (k *staticKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if cfg, ok := k.auths[res.RegistryStr()]; ok {
		return authn.FromConfig(cfg), nil
	}
	return authn.Anonymous, nil
}
