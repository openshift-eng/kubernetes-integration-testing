package collection

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
)

// Sane defaults with no hard technical constraint, adjust as needed.
const (
	// MaxScriptFiles is the maximum number of files allowed in a single script package.
	MaxScriptFiles = 32
	// MaxScriptFileSize is the maximum size of a single script file (1 MiB).
	MaxScriptFileSize = 1 << 20
	// MaxPackageSize is the maximum total size of all files in a single script package (4 MiB).
	MaxPackageSize = 4 << 20
	// MaxBundlePackages is the maximum number of packages in an agent bundle.
	MaxBundlePackages = 16
	// MaxBundleSize is the maximum total size of an agent bundle (64 MiB).
	MaxBundleSize = MaxBundlePackages * MaxPackageSize
	// InlineEntrypoint is the entrypoint path used for inline script packages.
	InlineEntrypoint = "inline"
)

type ScriptFile struct {
	Path    string
	Content []byte
}

type ScriptPackageContent struct {
	Name       string
	Entrypoint string
	Files      []ScriptFile
}

func LoadScriptPackage(pkg *v1alpha1.ScriptPackage, fsys fs.FS) (*ScriptPackageContent, error) {
	var files []ScriptFile
	var err error

	switch {
	case pkg.Root != "":
		if fsys == nil {
			return nil, fmt.Errorf("script package %q: root %q requires a collection source directory", pkg.Name, pkg.Root)
		}
		files, err = loadScriptDir(pkg.Root, pkg.Include, fsys)
	case pkg.Inline != "":
		if len(pkg.Inline) > MaxScriptFileSize {
			return nil, fmt.Errorf("script package %q: inline content exceeds maximum size (%d bytes)", pkg.Name, MaxScriptFileSize)
		}
		files = []ScriptFile{{Path: InlineEntrypoint, Content: []byte(pkg.Inline)}}
	default:
		return nil, fmt.Errorf("script package %q: one of root or inline must be set", pkg.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("script package %q: %w", pkg.Name, err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("script package %q: no files matched", pkg.Name)
	}

	entrypoint := pkg.Entrypoint
	if pkg.Inline != "" {
		entrypoint = InlineEntrypoint
	}

	if entrypoint != "" {
		found := false
		for _, f := range files {
			if f.Path == entrypoint {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("script package %q: entrypoint %q not found in package files", pkg.Name, entrypoint)
		}
	}

	return &ScriptPackageContent{
		Name:       pkg.Name,
		Entrypoint: entrypoint,
		Files:      files,
	}, nil
}

func loadScriptDir(root string, include []string, fsys fs.FS) ([]ScriptFile, error) {
	if containsTraversal(root) {
		return nil, fmt.Errorf("root %q contains path traversal", root)
	}

	sub, err := fs.Sub(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("opening root %q: %w", root, err)
	}

	var files []ScriptFile
	var totalSize int64

	err = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if path == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("symlink %q not allowed in script package", path)
		}
		if containsTraversal(path) {
			return fmt.Errorf("path %q contains traversal", path)
		}

		if len(include) > 0 {
			matched, err := matchesAny(path, include)
			if err != nil {
				return fmt.Errorf("matching %q: %w", path, err)
			}
			if !matched {
				return nil
			}
		}

		if len(files) >= MaxScriptFiles {
			return fmt.Errorf("exceeds maximum file count (%d)", MaxScriptFiles)
		}

		data, err := fs.ReadFile(sub, path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if len(data) > MaxScriptFileSize {
			return fmt.Errorf("file %s exceeds maximum size (%d bytes)", path, MaxScriptFileSize)
		}

		totalSize += int64(len(data))
		if totalSize > int64(MaxPackageSize) {
			return fmt.Errorf("package exceeds maximum total size (%d bytes)", MaxPackageSize)
		}

		files = append(files, ScriptFile{Path: path, Content: data})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func containsTraversal(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func matchesAny(path string, patterns []string) (bool, error) {
	for _, pattern := range patterns {
		matched, err := matchGlob(pattern, path)
		if err != nil {
			return false, fmt.Errorf("invalid glob %q: %w", pattern, err)
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

// matchGlob matches path against pattern, supporting ** as a recursive
// wildcard that matches zero or more directory segments.
func matchGlob(pattern, path string) (bool, error) {
	patParts := strings.Split(filepath.ToSlash(pattern), "/")
	pathParts := strings.Split(filepath.ToSlash(path), "/")
	return matchParts(patParts, pathParts)
}

func matchParts(patParts, pathParts []string) (bool, error) {
	for len(patParts) > 0 && len(pathParts) > 0 {
		if patParts[0] == "**" {
			rest := patParts[1:]
			for i := 0; i <= len(pathParts); i++ {
				ok, err := matchParts(rest, pathParts[i:])
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		}

		ok, err := filepath.Match(patParts[0], pathParts[0])
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		patParts = patParts[1:]
		pathParts = pathParts[1:]
	}

	for len(patParts) > 0 && patParts[0] == "**" {
		patParts = patParts[1:]
	}

	return len(patParts) == 0 && len(pathParts) == 0, nil
}
