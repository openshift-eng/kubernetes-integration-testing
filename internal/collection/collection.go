package collection

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

//go:embed builtin
var builtinFS embed.FS

type RenderContext struct {
	FeatureSet      string
	APIServerURL    string
	Platform        string
	ServiceNetworks []string
	NetworkType     string
}

type Collection struct {
	name      string
	templates map[string]*template.Template
}

func (c *Collection) Name() string {
	return c.name
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

func (c *Collection) Render(ctx RenderContext) ([]*unstructured.Unstructured, error) {
	names := make([]string, 0, len(c.templates))
	for name := range c.templates {
		names = append(names, name)
	}
	sort.Strings(names)

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
		objects = append(objects, objs...)
	}
	return objects, nil
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
