package cluster

import "context"

type Cluster interface {
	KubeConfig() string
	Teardown(ctx context.Context, log func(string)) error
}

type Provider interface {
	Create(ctx context.Context, opts CreateOpts, log func(string)) (Cluster, error)
	Destroy(ctx context.Context, name string, log func(string)) error
	Get(name string) (Cluster, bool)
}

type CreateOpts struct {
	Name    string
	Version string
}
