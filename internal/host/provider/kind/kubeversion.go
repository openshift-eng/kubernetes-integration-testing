package kind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
)

// resolveKubeVersion returns the exact kindest/node version to run for
// the requested version: an exact one must exist as a published tag, a
// partial one resolves to the latest published patch of that minor.
func resolveKubeVersion(ctx context.Context, requested string) (string, error) {
	v, err := kube.ParseVersion(requested)
	if err != nil {
		return "", fmt.Errorf("parsing kube version: %w", err)
	}

	tags, err := fetchKindestTags(ctx, v)
	if err != nil {
		return "", err
	}

	return resolveFromTags(v, tags)
}

func resolveFromTags(requested kube.Version, tags []string) (string, error) {
	minor := kube.Version{Major: requested.Major, Minor: requested.Minor, Patch: -1}

	var available []kube.Version
	for _, tag := range tags {
		tv, err := kube.ParseVersion(tag)
		if err != nil || !tv.IsExact() || !minor.Matches(tv) {
			continue
		}
		available = append(available, tv)
	}
	if len(available) == 0 {
		return "", fmt.Errorf("no kindest/node image for Kubernetes %s", minor)
	}

	sort.Slice(available, func(i, j int) bool { return available[i].Patch < available[j].Patch })

	if requested.IsExact() {
		for _, v := range available {
			if v == requested {
				return v.String(), nil
			}
		}
		return "", fmt.Errorf("no kindest/node image for Kubernetes %s; available: %s", requested, joinVersions(available))
	}

	return available[len(available)-1].String(), nil
}

func joinVersions(versions []kube.Version) string {
	ss := make([]string, 0, len(versions))
	for _, v := range versions {
		ss = append(ss, v.String())
	}
	return strings.Join(ss, ", ")
}

type hubTagList struct {
	Next    string `json:"next"`
	Results []struct {
		Name string `json:"name"`
	} `json:"results"`
}

// fetchKindestTags lists the published kindest/node tags for the minor
// of the requested version.
func fetchKindestTags(ctx context.Context, requested kube.Version) ([]string, error) {
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/kindest/node/tags?name=v%d.%d&page_size=100", requested.Major, requested.Minor)

	client := &http.Client{Timeout: 30 * time.Second}
	var tags []string
	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("listing kindest/node tags: %w", err)
		}
		var list hubTagList
		err = json.NewDecoder(resp.Body).Decode(&list)
		err = errors.Join(err, resp.Body.Close())
		if err != nil {
			return nil, fmt.Errorf("listing kindest/node tags: %w", err)
		}
		for _, r := range list.Results {
			tags = append(tags, r.Name)
		}
		url = list.Next
	}
	return tags, nil
}
