package release

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	cvoManifestsDir     = "manifests/"
	releaseManifestsDir = "release-manifests/"

	featureSetAnnotation  = "release.openshift.io/feature-set"
	clusterProfile        = "self-managed-high-availability"
)

type Manifest struct {
	Filename string
	Obj      *unstructured.Unstructured
}

type ReleaseManifests struct {
	mu       sync.Mutex
	registry *image.Registry
	log      *slog.Logger

	img   string
	files map[string]string
	ver   string
}

func NewReleaseManifests(registry *image.Registry, log *slog.Logger) *ReleaseManifests {
	return &ReleaseManifests{registry: registry, log: log}
}

func (rm *ReleaseManifests) Load(img string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if rm.img == img {
		return nil
	}

	allFiles := make(map[string]string)
	for _, dir := range []string{cvoManifestsDir, releaseManifestsDir} {
		files, err := rm.registry.ExtractDir(img, dir)
		if err != nil {
			return fmt.Errorf("extracting %s: %w", dir, err)
		}
		for k, v := range files {
			allFiles[dir+k] = v
		}
	}

	rm.img = img
	rm.files = allFiles
	rm.ver = releaseVersion(allFiles, rm.log)

	rm.log.Info("loaded release", "image", img, "files", len(allFiles), "version", rm.ver)
	return nil
}

func (rm *ReleaseManifests) All() ([]Manifest, error) {
	rm.mu.Lock()
	files := rm.files
	rm.mu.Unlock()

	return loadManifests(files)
}

func (rm *ReleaseManifests) Version() string {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.ver
}

func SplitCRDs(manifests []Manifest) (crds, rest []Manifest) {
	seen := make(map[string]int)
	for _, m := range manifests {
		gvk := m.Obj.GroupVersionKind()
		if strings.HasSuffix(gvk.Group, "apiextensions.k8s.io") && gvk.Kind == "CustomResourceDefinition" {
			name := m.Obj.GetName()
			if idx, ok := seen[name]; ok {
				crds[idx] = m
			} else {
				seen[name] = len(crds)
				crds = append(crds, m)
			}
		} else {
			rest = append(rest, m)
		}
	}
	return
}

func FilterResources(nonCRDs []Manifest, featureSet string, includes []string) (namespaces, resources []Manifest, err error) {
	patterns, err := compilePatterns(includes)
	if err != nil {
		return nil, nil, err
	}
	ns, filtered := splitNamespaces(FilterByFeatureSet(nonCRDs, featureSet))
	return ns, filterByIncludes(filtered, patterns), nil
}

func FindFeatureGateStatus(manifests []Manifest, featureSet string) map[string]interface{} {
	if featureSet == "" {
		featureSet = "Default"
	}
	for _, m := range manifests {
		if m.Obj.GetKind() != "FeatureGate" || m.Obj.GetName() != "cluster" {
			continue
		}
		annotations := m.Obj.GetAnnotations()
		fsValue, ok := annotations["release.openshift.io/feature-set"]
		if !ok || fsValue != featureSet {
			continue
		}
		status, ok := m.Obj.Object["status"].(map[string]interface{})
		if ok {
			return status
		}
	}
	return nil
}

func loadManifests(files map[string]string) ([]Manifest, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []Manifest
	for _, name := range names {
		if !isManifestFile(name) {
			continue
		}
		data, err := os.ReadFile(files[name])
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		objs, err := parseMultiDoc(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		for _, obj := range objs {
			out = append(out, Manifest{Filename: name, Obj: obj})
		}
	}
	return out, nil
}

func FilterByFeatureSet(manifests []Manifest, featureSet string) []Manifest {
	var out []Manifest
	for _, m := range manifests {
		if includeManifest(m.Obj.GetAnnotations(), featureSet) {
			out = append(out, m)
		}
	}
	return out
}

func parseMultiDoc(data []byte) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if obj.GetAPIVersion() == "" || obj.GetKind() == "" {
			continue
		}
		objects = append(objects, obj)
	}
	return objects, nil
}

func IsDelete(m Manifest) bool {
	return m.Obj.GetAnnotations()["release.openshift.io/delete"] == "true"
}

func MatchesProfile(annotations map[string]string, featureSet string) bool {
	if annotations == nil {
		return false
	}
	profileKey := fmt.Sprintf("include.release.openshift.io/%s", clusterProfile)
	if v, ok := annotations[profileKey]; !ok || v != "true" {
		return false
	}
	fsValue, hasFS := annotations[featureSetAnnotation]
	if !hasFS {
		return true
	}
	required := featureSet
	if required == "" {
		required = "Default"
	}
	for _, v := range strings.Split(fsValue, ",") {
		if strings.TrimSpace(v) == required {
			return true
		}
	}
	return false
}

func includeManifest(annotations map[string]string, featureSet string) bool {
	if annotations["release.openshift.io/delete"] == "true" {
		return false
	}
	return MatchesProfile(annotations, featureSet)
}

func filterByIncludes(resources []Manifest, patterns []*regexp.Regexp) []Manifest {
	if len(patterns) == 0 {
		return nil
	}
	var out []Manifest
	for _, m := range resources {
		for _, p := range patterns {
			if p.MatchString(m.Filename) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
		compiled[i] = re
	}
	return compiled, nil
}

func splitNamespaces(resources []Manifest) (namespaces, rest []Manifest) {
	for _, m := range resources {
		if m.Obj.GetKind() == "Namespace" {
			namespaces = append(namespaces, m)
		} else {
			rest = append(rest, m)
		}
	}
	return
}

func releaseVersion(files map[string]string, log *slog.Logger) string {
	path, ok := files[releaseManifestsDir+"image-references"]
	if !ok {
		log.Warn("image-references not found, cannot determine release version")
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warn("reading image-references", "error", err)
		return ""
	}
	var m struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		log.Warn("parsing image-references", "error", err)
		return ""
	}
	return m.Metadata.Name
}

func isManifestFile(name string) bool {
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}
