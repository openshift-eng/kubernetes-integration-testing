package imageref

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/distribution/reference"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
)

type Requested struct {
	named reference.Named
}

func ParseRequested(raw string) (Requested, error) {
	named, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return Requested{}, fmt.Errorf("parsing image reference %s: %w", raw, err)
	}
	return Requested{named: named}, nil
}

func (r Requested) String() string {
	if r.named == nil {
		return ""
	}
	return r.named.String()
}

type Resolved struct {
	canonical reference.Canonical
}

func Parse(s string) (Resolved, error) {
	parsed, err := reference.ParseAnyReference(s)
	if err != nil {
		return Resolved{}, fmt.Errorf("parsing image reference %s: %w", s, err)
	}
	canonical, ok := parsed.(reference.Canonical)
	if !ok {
		return Resolved{}, fmt.Errorf("%s is not a canonical digest reference", s)
	}
	normalized, err := reference.WithDigest(reference.TrimNamed(canonical), canonical.Digest())
	if err != nil {
		return Resolved{}, fmt.Errorf("normalizing canonical reference %s: %w", s, err)
	}
	return Resolved{canonical: normalized}, nil
}

func (r Resolved) String() string {
	if r.canonical == nil {
		return ""
	}
	return r.canonical.String()
}

func (r Resolved) Digest() string {
	if r.canonical == nil {
		return ""
	}
	return r.canonical.Digest().String()
}

type Resolver interface {
	Resolve(ctx context.Context) (Resolved, error)
}

// podmanResolver resolves a configured image reference to a canonical
// repo@digest form using podman. It caches the first successful result
// for the lifetime of the process; failed attempts are retried.
type podmanResolver struct {
	client    podman.Client
	requested Requested
	log       *slog.Logger
	mu        sync.Mutex
	resolved  Resolved
	ok        bool
}

func NewPodmanResolver(client podman.Client, requested Requested, log *slog.Logger) Resolver {
	return &podmanResolver{client: client, requested: requested, log: log}
}

func (r *podmanResolver) Resolve(ctx context.Context) (Resolved, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ok {
		if _, err := r.ensureImage(ctx, r.resolved.String()); err != nil {
			return Resolved{}, fmt.Errorf("cached image no longer available: %w", err)
		}
		return r.resolved, nil
	}

	resolved, err := r.resolve(ctx)
	if err != nil {
		return Resolved{}, err
	}

	r.resolved = resolved
	r.ok = true
	r.log.Info("resolved controller image", "requested", r.requested.String(), "ref", resolved.String())
	return resolved, nil
}

func (r *podmanResolver) resolve(ctx context.Context) (Resolved, error) {
	image := r.requested.String()

	info, err := r.ensureImage(ctx, image)
	if err != nil {
		return Resolved{}, err
	}

	if info.Digest == "" {
		return Resolved{}, fmt.Errorf("image %s has no manifest digest", image)
	}

	if canonical, ok := r.requested.named.(reference.Canonical); ok {
		if canonical.Digest().String() != info.Digest {
			return Resolved{}, fmt.Errorf("image %s: requested digest %s but local image has %s", image, canonical.Digest().String(), info.Digest)
		}
		normalized, err := reference.WithDigest(reference.TrimNamed(canonical), canonical.Digest())
		if err != nil {
			return Resolved{}, fmt.Errorf("normalizing canonical reference %s: %w", image, err)
		}
		return Resolved{canonical: normalized}, nil
	}

	ref, err := matchRepoDigest(r.requested.named, info.RepoDigests, info.Digest)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolving digest for %s: %w", image, err)
	}
	return ref, nil
}

func (r *podmanResolver) ensureImage(ctx context.Context, image string) (*podman.ImageInfo, error) {
	info, err := r.client.ImageInspect(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("inspecting image %s: %w", image, err)
	}
	if info != nil {
		return info, nil
	}
	if err := r.client.Pull(ctx, image); err != nil {
		return nil, fmt.Errorf("pulling image %s: %w", image, err)
	}
	info, err = r.client.ImageInspect(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("inspecting image %s after pull: %w", image, err)
	}
	if info == nil {
		return nil, fmt.Errorf("image %s not found after pull", image)
	}
	return info, nil
}

func matchRepoDigest(named reference.Named, repoDigests []string, imageDigest string) (Resolved, error) {
	requestedDomain := reference.Domain(named)
	requestedPath := reference.Path(named)
	for _, d := range repoDigests {
		digestRef, err := reference.ParseAnyReference(d)
		if err != nil {
			continue
		}
		dn, ok := digestRef.(reference.Named)
		if !ok {
			continue
		}
		if reference.Domain(dn) != requestedDomain || reference.Path(dn) != requestedPath {
			continue
		}
		canonical, ok := digestRef.(reference.Canonical)
		if !ok {
			continue
		}
		if canonical.Digest().String() != imageDigest {
			continue
		}
		normalized, err := reference.WithDigest(reference.TrimNamed(canonical), canonical.Digest())
		if err != nil {
			continue
		}
		return Resolved{canonical: normalized}, nil
	}
	return Resolved{}, fmt.Errorf("no matching repo digest found")
}
