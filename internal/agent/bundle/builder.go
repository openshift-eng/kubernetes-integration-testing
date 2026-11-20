package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

type Manifest struct {
	Runtime      string                        `json:"runtime"`
	Timeout      *metav1.Duration              `json:"timeout,omitempty"`
	Entrypoint   string                        `json:"entrypoint"`
	Packages     []PackageManifest             `json:"packages"`
	Capabilities []collection.AtomicCapability `json:"capabilities,omitempty"`
}

type PackageManifest struct {
	Name       string   `json:"name"`
	Entrypoint string   `json:"entrypoint,omitempty"`
	Files      []string `json:"files"`
}

type Bundle struct {
	Digest   string
	Manifest Manifest
	data     []byte
}

func (b *Bundle) Reader() io.Reader { return bytes.NewReader(b.data) }
func (b *Bundle) Size() int         { return len(b.data) }

type BuildInput struct {
	Packages              []collection.ScriptPackageContent
	EntrypointPackage     string
	Values                map[string]json.RawMessage
	Runtime               string
	Timeout               *metav1.Duration
	EffectiveCapabilities []collection.AtomicCapability
}

func Build(input BuildInput) (*Bundle, error) {
	sorted, err := validateAndSort(input.Packages)
	if err != nil {
		return nil, err
	}

	var caps []collection.AtomicCapability
	if len(input.EffectiveCapabilities) > 0 {
		caps = input.EffectiveCapabilities
	}

	manifest, err := buildManifest(sorted, input.EntrypointPackage, input.Runtime, input.Timeout, caps)
	if err != nil {
		return nil, err
	}

	data, err := packTarGz(sorted, manifest, input.Values)
	if err != nil {
		return nil, err
	}

	if len(data) > collection.MaxBundleSize {
		return nil, fmt.Errorf("bundle size %d exceeds maximum %d", len(data), collection.MaxBundleSize)
	}

	digest := sha256.Sum256(data)
	return &Bundle{
		Digest:   fmt.Sprintf("%x", digest),
		Manifest: *manifest,
		data:     data,
	}, nil
}

func validateAndSort(packages []collection.ScriptPackageContent) ([]collection.ScriptPackageContent, error) {
	if len(packages) == 0 {
		return nil, fmt.Errorf("no packages provided")
	}
	if len(packages) > collection.MaxBundlePackages {
		return nil, fmt.Errorf("too many packages (%d), maximum is %d", len(packages), collection.MaxBundlePackages)
	}

	sorted := make([]collection.ScriptPackageContent, len(packages))
	copy(sorted, packages)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i := range sorted {
		if i > 0 && sorted[i].Name == sorted[i-1].Name {
			return nil, fmt.Errorf("duplicate package name %q", sorted[i].Name)
		}
		files := make([]collection.ScriptFile, len(sorted[i].Files))
		copy(files, sorted[i].Files)
		sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
		sorted[i].Files = files
	}
	return sorted, nil
}

func buildManifest(packages []collection.ScriptPackageContent, entrypointPackage string, runtime string, timeout *metav1.Duration, caps []collection.AtomicCapability) (*Manifest, error) {
	var entrypointFile string
	for _, pkg := range packages {
		if pkg.Name == entrypointPackage {
			entrypointFile = pkg.Entrypoint
			break
		}
	}
	if entrypointFile == "" {
		return nil, fmt.Errorf("entrypoint package %q not found or has no entrypoint", entrypointPackage)
	}

	manifest := &Manifest{
		Runtime:      runtime,
		Timeout:      timeout,
		Entrypoint:   entrypointPackage + "/" + entrypointFile,
		Packages:     make([]PackageManifest, len(packages)),
		Capabilities: caps,
	}
	for i, pkg := range packages {
		files := make([]string, len(pkg.Files))
		for j, f := range pkg.Files {
			files[j] = f.Path
		}
		manifest.Packages[i] = PackageManifest{
			Name:       pkg.Name,
			Entrypoint: pkg.Entrypoint,
			Files:      files,
		}
	}
	return manifest, nil
}

func packTarGz(packages []collection.ScriptPackageContent, manifest *Manifest, values map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	if err != nil {
		return nil, fmt.Errorf("creating gzip writer: %w", err)
	}
	gw.ModTime = time.Time{}
	tw := tar.NewWriter(gw)

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshaling manifest: %w", err)
	}
	if err := writeTarEntry(tw, "manifest.json", manifestJSON); err != nil {
		return nil, err
	}

	valuesJSON, err := marshalValues(values)
	if err != nil {
		return nil, fmt.Errorf("marshaling values: %w", err)
	}
	if err := writeTarEntry(tw, "values.json", valuesJSON); err != nil {
		return nil, err
	}

	for _, pkg := range packages {
		for _, f := range pkg.Files {
			if err := writeTarEntry(tw, pkg.Name+"/"+f.Path, f.Content); err != nil {
				return nil, err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing tar writer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("closing gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}

func writeTarEntry(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Size:    int64(len(data)),
		Mode:    0644,
		ModTime: time.Unix(0, 0),
		Format:  tar.FormatPAX,
	}); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("writing tar data for %s: %w", name, err)
	}
	return nil
}

func marshalValues(values map[string]json.RawMessage) ([]byte, error) {
	if values == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(values)
}
