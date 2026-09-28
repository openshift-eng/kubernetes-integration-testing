package cluster

import (
	"context"
)

type Cluster interface {
	KubeConfig() string
	Teardown(ctx context.Context) error
}

type Provider interface {
	Create(ctx context.Context, opts CreateOpts) (Cluster, error)
	Destroy(ctx context.Context, name string) error
	Get(name string) (Cluster, bool)
}

type CreateOpts struct {
	Name       string
	Version    string
	Workers    int
	PullSecret string
}
