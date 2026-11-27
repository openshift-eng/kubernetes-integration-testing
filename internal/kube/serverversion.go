package kube

import (
	"context"
	"fmt"
	"strings"
	"time"
)

var (
	serverVersionRetryInterval = 500 * time.Millisecond
	serverVersionRetryTimeout  = 10 * time.Second
)

// ServerKubeVersion returns the Kubernetes version the cluster API
// server reports, as a canonical vX.Y.Z. Transport errors are retried
// briefly; a version that cannot be canonicalized fails immediately.
func ServerKubeVersion(ctx context.Context, clients *Clients) (string, error) {
	deadline := time.Now().Add(serverVersionRetryTimeout)
	for {
		info, err := clients.Discovery.ServerVersion()
		if err == nil {
			return normalizeServerVersion(info.GitVersion)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("getting API server version (retried for %s): %w", serverVersionRetryTimeout, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(serverVersionRetryInterval):
		}
	}
}

// normalizeServerVersion canonicalizes the API server gitVersion,
// dropping any build metadata.
func normalizeServerVersion(gitVersion string) (string, error) {
	v, err := ParseVersion(strings.SplitN(gitVersion, "+", 2)[0])
	if err != nil || !v.IsExact() {
		return "", fmt.Errorf("unexpected API server version %q", gitVersion)
	}
	return v.String(), nil
}
