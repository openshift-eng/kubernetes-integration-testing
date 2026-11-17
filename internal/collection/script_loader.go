package collection

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
)

// ScriptCollectionResolver looks up ResourceCollections by name.
// Implementations typically wrap a Kubernetes API client.
type ScriptCollectionResolver interface {
	Resolve(ctx context.Context, name string) (*v1alpha1.ResourceCollection, error)
}

// ScriptLoader loads script packages from collections, resolving source
// filesystems and cross-collection dependencies. It keeps filesystem
// resolution private so callers never handle raw fs.FS.
type ScriptLoader struct {
	collectionsDir string
}

// LoadResult contains the fully resolved script packages from a collection
// and its dependencies, ready to pass to bundle.Build.
type LoadResult struct {
	Packages      []ScriptPackageContent
	LocalPackages map[string]bool
	ContentHash   string
}

// NewScriptLoader creates a loader. collectionsDir is the on-disk root
// for user-provided collections; pass "" to use only builtins.
func NewScriptLoader(collectionsDir string) *ScriptLoader {
	return &ScriptLoader{collectionsDir: collectionsDir}
}

// Load resolves all script packages for the named collection and its
// scriptCollections dependencies, returning them with a canonical
// content hash.
func (l *ScriptLoader) Load(
	ctx context.Context,
	collectionName string,
	resolver ScriptCollectionResolver,
) (*LoadResult, error) {
	rc, err := resolver.Resolve(ctx, collectionName)
	if err != nil {
		return nil, fmt.Errorf("resolving collection %q: %w", collectionName, err)
	}

	var all []sourcePackage
	localPackages := make(map[string]bool, len(rc.Spec.Scripts))

	collFS, err := l.resolveFS(rc.Spec.Source)
	if err != nil {
		return nil, err
	}
	for i := range rc.Spec.Scripts {
		pkg := &rc.Spec.Scripts[i]
		content, err := LoadScriptPackage(pkg, collFS)
		if err != nil {
			return nil, fmt.Errorf("loading script package %q from collection %q: %w", pkg.Name, rc.Name, err)
		}
		all = append(all, sourcePackage{collection: rc.Name, content: *content})
		localPackages[pkg.Name] = true
	}

	visited := map[string]bool{collectionName: true}
	var walkDeps func(deps []string, path []string) error
	walkDeps = func(deps []string, path []string) error {
		for _, refName := range deps {
			for _, p := range path {
				if p == refName {
					return fmt.Errorf("script collection cycle: %s", strings.Join(append(path, refName), " -> "))
				}
			}
			if visited[refName] {
				continue
			}
			visited[refName] = true

			ref, err := resolver.Resolve(ctx, refName)
			if err != nil {
				return fmt.Errorf("resolving referenced collection %q: %w", refName, err)
			}
			refFS, err := l.resolveFS(ref.Spec.Source)
			if err != nil {
				return err
			}
			for i := range ref.Spec.Scripts {
				pkg := &ref.Spec.Scripts[i]
				content, err := LoadScriptPackage(pkg, refFS)
				if err != nil {
					return fmt.Errorf("loading script package %q from collection %q: %w", pkg.Name, ref.Name, err)
				}
				all = append(all, sourcePackage{collection: ref.Name, content: *content})
			}
			if err := walkDeps(ref.Spec.ScriptCollections, append(path, refName)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walkDeps(rc.Spec.ScriptCollections, []string{collectionName}); err != nil {
		return nil, err
	}

	seen := make(map[string]string, len(all))
	for _, sp := range all {
		if prev, dup := seen[sp.content.Name]; dup {
			return nil, fmt.Errorf("duplicate script package %q: provided by collection %q and %q", sp.content.Name, prev, sp.collection)
		}
		seen[sp.content.Name] = sp.collection
	}

	packages := make([]ScriptPackageContent, len(all))
	for i, sp := range all {
		packages[i] = sp.content
	}

	hash := scriptContentHash(packages, localPackages)

	return &LoadResult{
		Packages:      packages,
		LocalPackages: localPackages,
		ContentHash:   hash,
	}, nil
}

// resolveFS returns the filesystem for a collection source, trying the
// user-provided directory first and falling back to builtins. Returns
// nil when source is empty (inline/local-only collections).
func (l *ScriptLoader) resolveFS(source string) (fs.FS, error) {
	if source == "" {
		return nil, nil
	}
	var userFS fs.FS
	if l.collectionsDir != "" {
		userFS = os.DirFS(l.collectionsDir)
	}
	return resolveCollectionFS(source, userFS)
}

// scriptContentHash produces a canonical hash over the resolved script
// content. It covers what would end up in the bundle plus local
// package membership: for each package (sorted by name) the package
// name, declared entrypoint, whether it is a local package, and each
// file's relative path + content (sorted by path). Collection names
// and dependency order are excluded so that reorganising collections
// without changing scripts produces the same hash, but moving a
// package from local to dependency does change it.
func scriptContentHash(packages []ScriptPackageContent, localPackages map[string]bool) string {
	if len(packages) == 0 {
		return EmptyHash
	}
	sorted := make([]ScriptPackageContent, len(packages))
	copy(sorted, packages)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	var lenBuf [4]byte
	writeField := func(b []byte) {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
		h.Write(lenBuf[:])
		h.Write(b)
	}
	writeUint32 := func(v int) {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(v))
		h.Write(lenBuf[:])
	}

	writeUint32(len(sorted))
	for _, pkg := range sorted {
		writeField([]byte(pkg.Name))
		writeField([]byte(pkg.Entrypoint))
		if localPackages[pkg.Name] {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}

		files := make([]ScriptFile, len(pkg.Files))
		copy(files, pkg.Files)
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		writeUint32(len(files))
		for _, f := range files {
			writeField([]byte(f.Path))
			writeField(f.Content)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

type sourcePackage struct {
	collection string
	content    ScriptPackageContent
}
