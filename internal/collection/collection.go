package collection

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

const ContentHashAnnotation = "kit.openshift.io/content-hash"

//go:embed builtin
var builtinFS embed.FS

type RenderResult struct {
	Objects []*unstructured.Unstructured
	Hash    string
}

type RenderContext struct {
	FeatureSet      string
	APIServerURL    string
	Platform        string
	ServiceNetworks []string
	NetworkType     string
	Values          map[string]string
}

type Collection struct {
	name      string
	templates map[string]*template.Template
}

func (c *Collection) Name() string {
	return c.name
}

// BuiltinNames returns the names of all embedded builtin collections.
func BuiltinNames() []string {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// Load resolves a collection by name. If userFS is non-nil and contains a
// subdirectory matching name, it takes precedence over the embedded builtins.
func Load(name string, userFS fs.FS) (*Collection, error) {
	if userFS != nil {
		sub, err := fs.Sub(userFS, name)
		if err == nil {
			if entries, err := fs.ReadDir(sub, "."); err == nil && len(entries) > 0 {
				return loadFromFS(name, sub)
			}
		}
	}
	sub, err := fs.Sub(builtinFS, filepath.Join("builtin", name))
	if err != nil {
		return nil, fmt.Errorf("collection %q not found: %w", name, err)
	}
	return loadFromFS(name, sub)
}

// LoadInline creates a collection from inline template content.
func LoadInline(name string, contents []string) (*Collection, error) {
	templates := make(map[string]*template.Template)
	for i, content := range contents {
		key := fmt.Sprintf("inline-%d", i)
		tmpl, err := template.New(key).Parse(content)
		if err != nil {
			return nil, fmt.Errorf("parsing inline template %d: %w", i, err)
		}
		templates[key] = tmpl
	}
	if len(templates) == 0 {
		return nil, fmt.Errorf("collection %q has no templates", name)
	}
	return &Collection{name: name, templates: templates}, nil
}

// BuiltinFS returns the embedded filesystem for a builtin collection.
func BuiltinFS(name string) (fs.FS, error) {
	return fs.Sub(builtinFS, filepath.Join("builtin", name))
}

// LoadSpec scans fsys for a ResourceCollection CR whose metadata.name
// matches the given name. Returns nil if not found.
func LoadSpec(name string, fsys fs.FS) *v1alpha1.ResourceCollectionSpec {
	return findSpec(name, fsys)
}

func findSpec(name string, fsys fs.FS) *v1alpha1.ResourceCollectionSpec {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			continue
		}
		obj, _, err := kube.Decoder.Decode(data, nil, nil)
		if err != nil {
			continue
		}
		rc, ok := obj.(*v1alpha1.ResourceCollection)
		if ok && rc.Name == name {
			return &rc.Spec
		}
	}
	return nil
}

func isKitResource(data []byte) bool {
	obj, _, err := kube.Decoder.Decode(data, nil, nil)
	if err != nil {
		return false
	}
	return strings.HasPrefix(obj.GetObjectKind().GroupVersionKind().Group, "kit.openshift.io")
}

func loadFromFS(name string, fsys fs.FS) (*Collection, error) {
	templates := make(map[string]*template.Template)

	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		if isKitResource(data) {
			return nil
		}

		tmpl, err := template.New(path).Parse(string(data))
		if err != nil {
			return fmt.Errorf("parsing template %s: %w", path, err)
		}

		templates[path] = tmpl
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("loading collection %q: %w", name, err)
	}

	if len(templates) == 0 {
		return nil, fmt.Errorf("collection %q has no templates", name)
	}

	return &Collection{name: name, templates: templates}, nil
}

func (c *Collection) Render(ctx RenderContext) (*RenderResult, error) {
	names := make([]string, 0, len(c.templates))
	for name := range c.templates {
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	var objects []*unstructured.Unstructured
	for _, name := range names {
		tmpl := c.templates[name]
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			return nil, fmt.Errorf("rendering %s: %w", name, err)
		}

		objs, err := parseYAML(buf.Bytes())
		if err != nil {
			return nil, fmt.Errorf("parsing rendered %s: %w", name, err)
		}
		for _, obj := range objs {
			b, _ := json.Marshal(obj.Object)
			h.Write(b)
		}
		objects = append(objects, objs...)
	}
	return &RenderResult{
		Objects: objects,
		Hash:    hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func parseYAML(data []byte) ([]*unstructured.Unstructured, error) {
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
