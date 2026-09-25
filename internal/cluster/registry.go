package cluster

import (
	"context"
	"fmt"
)

type Registry struct {
	providers      map[string]Provider
	defaultProvider string
}

func NewRegistry(defaultProvider string, providers map[string]Provider) *Registry {
	return &Registry{
		providers:      providers,
		defaultProvider: defaultProvider,
	}
}

func (r *Registry) Create(ctx context.Context, providerName string, opts CreateOpts) (Cluster, error) {
	if _, pname, ok := r.Get(opts.Name); ok {
		return nil, fmt.Errorf("cluster %q already exists (provider %s)", opts.Name, pname)
	}

	p, err := r.provider(providerName)
	if err != nil {
		return nil, err
	}

	return p.Create(ctx, opts)
}

func (r *Registry) Destroy(ctx context.Context, name string) error {
	for _, p := range r.providers {
		if _, ok := p.Get(name); ok {
			return p.Destroy(ctx, name)
		}
	}
	return fmt.Errorf("cluster %q not found", name)
}

func (r *Registry) Get(name string) (Cluster, string, bool) {
	for pname, p := range r.providers {
		if c, ok := p.Get(name); ok {
			return c, pname, true
		}
	}
	return nil, "", false
}

func (r *Registry) provider(name string) (Provider, error) {
	if name == "" {
		name = r.defaultProvider
	}
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	return p, nil
}
