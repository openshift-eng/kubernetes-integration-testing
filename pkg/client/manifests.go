package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Manifest is a single parsed Kubernetes manifest.
type Manifest struct {
	// JSON is the manifest serialized as a single JSON document.
	JSON []byte
	// Kind, Name and Namespace identify the manifest in errors and logs.
	Kind      string
	Name      string
	Namespace string
}

// ResourceSource supplies ordered manifests to apply when a cluster is
// created.
type ResourceSource interface {
	Load() ([]Manifest, error)
}

type fileSource []string

// FromFiles loads manifests from paths, in order. Each path may be a
// file containing one or more YAML/JSON documents, a directory (its
// .yaml, .yml and .json files are loaded in lexical order), or "-" for
// stdin.
func FromFiles(paths ...string) ResourceSource {
	return fileSource(paths)
}

func (s fileSource) Load() ([]Manifest, error) {
	var out []Manifest
	for _, path := range s {
		if path == "-" {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, fmt.Errorf("reading stdin: %w", err)
			}
			ms, err := parseDocs(raw, "stdin")
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
			continue
		}

		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}

		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, fmt.Errorf("reading directory %s: %w", path, err)
			}
			var names []string
			for _, e := range entries {
				if e.IsDir() || !isManifestFile(e.Name()) {
					continue
				}
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, name := range names {
				full := filepath.Join(path, name)
				raw, err := os.ReadFile(full)
				if err != nil {
					return nil, fmt.Errorf("reading %s: %w", full, err)
				}
				ms, err := parseDocs(raw, full)
				if err != nil {
					return nil, err
				}
				out = append(out, ms...)
			}
			continue
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		ms, err := parseDocs(raw, path)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

type fsSource struct {
	fsys     fs.FS
	patterns []string
}

// FromFS loads manifests matching glob patterns from an fs.FS such as
// embed.FS. Within each pattern, matches are loaded in lexical order and
// directories are skipped.
func FromFS(fsys fs.FS, patterns ...string) ResourceSource {
	return fsSource{fsys: fsys, patterns: patterns}
}

func (s fsSource) Load() ([]Manifest, error) {
	var out []Manifest
	for _, pattern := range s.patterns {
		matches, err := fs.Glob(s.fsys, pattern)
		if err != nil {
			return nil, fmt.Errorf("globbing %s: %w", pattern, err)
		}
		sort.Strings(matches)
		for _, match := range matches {
			info, err := fs.Stat(s.fsys, match)
			if err != nil {
				return nil, fmt.Errorf("stat %s: %w", match, err)
			}
			if info.IsDir() {
				continue
			}
			raw, err := fs.ReadFile(s.fsys, match)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", match, err)
			}
			ms, err := parseDocs(raw, match)
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
		}
	}
	return out, nil
}

type stringSource string

// FromString parses manifests from an inline multi-document YAML/JSON
// string.
func FromString(raw string) ResourceSource {
	return stringSource(raw)
}

func (s stringSource) Load() ([]Manifest, error) {
	return parseDocs([]byte(s), "inline string")
}

type bytesSource [][]byte

// FromBytes parses manifests from raw multi-document YAML/JSON blobs.
func FromBytes(docs ...[]byte) ResourceSource {
	return bytesSource(docs)
}

func (s bytesSource) Load() ([]Manifest, error) {
	var out []Manifest
	for i, raw := range s {
		ms, err := parseDocs(raw, fmt.Sprintf("document set %d", i+1))
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

func isManifestFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

// parseDocs splits a multi-document YAML (or single JSON) blob into
// manifests, validating that each document carries an apiVersion, kind
// and name.
func parseDocs(raw []byte, origin string) ([]Manifest, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []Manifest
	for {
		var obj map[string]interface{}
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parsing %s: %w", origin, err)
		}
		if len(obj) == 0 {
			continue
		}
		data, err := json.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", origin, err)
		}
		m, err := manifestFromJSON(data, fmt.Sprintf("%s (document %d)", origin, len(out)+1))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func manifestFromJSON(data []byte, origin string) (Manifest, error) {
	var meta struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return Manifest{}, fmt.Errorf("parsing %s: %w", origin, err)
	}
	switch {
	case meta.APIVersion == "":
		return Manifest{}, fmt.Errorf("%s: manifest is missing apiVersion", origin)
	case meta.Kind == "":
		return Manifest{}, fmt.Errorf("%s: manifest is missing kind", origin)
	case meta.Metadata.Name == "":
		return Manifest{}, fmt.Errorf("%s: manifest is missing metadata.name", origin)
	}
	return Manifest{
		JSON:      data,
		Kind:      meta.Kind,
		Name:      meta.Metadata.Name,
		Namespace: meta.Metadata.Namespace,
	}, nil
}
