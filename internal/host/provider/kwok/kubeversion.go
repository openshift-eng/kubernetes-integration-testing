package kwok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
)

// resolveKubeVersion returns the exact Kubernetes version to run for
// the requested version: an exact one is used as is, a partial one
// resolves to the latest patch release of that minor.
func resolveKubeVersion(ctx context.Context, requested string) (string, error) {
	v, err := kube.ParseVersion(requested)
	if err != nil {
		return "", fmt.Errorf("parsing kube version: %w", err)
	}
	if v.IsExact() {
		return v.String(), nil
	}
	return fetchStableRelease(ctx, v)
}

// fetchStableRelease returns the latest patch release of a minor, per
// the Kubernetes release channel.
var fetchStableRelease = func(ctx context.Context, v kube.Version) (_ string, retErr error) {
	url := fmt.Sprintf("https://dl.k8s.io/release/stable-%d.%d.txt", v.Major, v.Minor)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolving Kubernetes %s: %w", v, err)
	}
	defer func() { retErr = errors.Join(retErr, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving Kubernetes %s: %s", v, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", fmt.Errorf("resolving Kubernetes %s: %w", v, err)
	}
	stable, err := kube.ParseVersion(strings.TrimSpace(string(body)))
	if err != nil || !stable.IsExact() {
		return "", fmt.Errorf("resolving Kubernetes %s: unexpected response %q", v, strings.TrimSpace(string(body)))
	}
	return stable.String(), nil
}
