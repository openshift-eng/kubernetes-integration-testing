package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/distribution/reference"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/naming"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
)

type PodmanControllerLauncher struct {
	client         podman.Client
	cacheDir       string
	collectionsDir string
	log            *slog.Logger
}

func NewPodmanControllerLauncher(client podman.Client, cacheDir, collectionsDir string, log *slog.Logger) *PodmanControllerLauncher {
	return &PodmanControllerLauncher{client: client, cacheDir: cacheDir, collectionsDir: collectionsDir, log: log}
}

func (l *PodmanControllerLauncher) ResolveImage(ctx context.Context, requested string) (string, error) {
	parsed, err := reference.ParseAnyReference(requested)
	if err != nil {
		return "", fmt.Errorf("parsing image reference %s: %w", requested, err)
	}

	if canonical, ok := parsed.(reference.Canonical); ok {
		normalized, err := reference.WithDigest(reference.TrimNamed(canonical), canonical.Digest())
		if err != nil {
			return "", fmt.Errorf("normalizing canonical reference %s: %w", requested, err)
		}
		ref := normalized.String()
		l.log.Info("resolved controller image", "requested", requested, "ref", ref)
		return ref, nil
	}

	info, err := l.client.ImageInspect(ctx, requested)
	if err != nil {
		return "", fmt.Errorf("inspecting image %s: %w", requested, err)
	}
	if info == nil {
		if err := l.client.Pull(ctx, requested); err != nil {
			return "", fmt.Errorf("pulling image %s: %w", requested, err)
		}
		info, err = l.client.ImageInspect(ctx, requested)
		if err != nil {
			return "", fmt.Errorf("inspecting image %s after pull: %w", requested, err)
		}
		if info == nil {
			return "", fmt.Errorf("image %s not found after pull", requested)
		}
	}

	if info.Digest == "" {
		return "", fmt.Errorf("image %s has no manifest digest", requested)
	}

	ref := matchRepoDigest(parsed, info.RepoDigests, info.Digest)
	if ref == "" {
		return "", fmt.Errorf("could not resolve digest for %s: no matching repo digest found", requested)
	}

	l.log.Info("resolved controller image", "requested", requested, "ref", ref)
	return ref, nil
}

func matchRepoDigest(parsed reference.Reference, repoDigests []string, imageDigest string) string {
	named, ok := parsed.(reference.Named)
	if !ok {
		return ""
	}
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
		return d
	}
	return ""
}

func (l *PodmanControllerLauncher) Launch(ctx context.Context, name string, c Cluster, image string) (func(), error) {
	containerName := naming.ContainerName(name)

	var containerID string

	existing, err := l.client.ContainerInspect(ctx, containerName)
	if err != nil {
		return nil, fmt.Errorf("inspecting controller container %s: %w", containerName, err)
	}
	if existing != nil {
		if err := l.handleExisting(existing, containerName, name, image); err != nil {
			return nil, err
		}
		containerID = existing.ID
	} else {
		containerID, err = l.create(ctx, name, c, image, containerName)
		if err != nil {
			return nil, err
		}
	}

	stop := func() {
		l.log.Info("stopping controller container", "name", containerName, "id", containerID)
		_ = l.client.Stop(context.Background(), containerID, 10)
		_ = l.client.Remove(context.Background(), containerID, true)
	}
	return stop, nil
}

func (l *PodmanControllerLauncher) handleExisting(info *podman.ContainerInfo, containerName, clusterName, image string) error {
	if info.Labels["kit.role"] != "controller" || info.Labels["kit.cluster"] != clusterName {
		return fmt.Errorf("container %s exists but belongs to a different cluster or role", containerName)
	}

	expectedDigest, err := extractDigest(image)
	if err != nil {
		return fmt.Errorf("extracting digest from stored image ref: %w", err)
	}
	if info.ImageDigest != expectedDigest {
		return fmt.Errorf(
			"controller container %s has digest %s but expected %s; recreate the cluster to use the new image",
			containerName, info.ImageDigest, expectedDigest,
		)
	}

	if info.Status != "running" {
		return fmt.Errorf(
			"controller container %s exists but is %s; recreate the cluster",
			containerName, info.Status,
		)
	}

	l.log.Info("adopting existing controller container", "name", containerName, "digest", expectedDigest)
	return nil
}

func extractDigest(imageRef string) (string, error) {
	parsed, err := reference.ParseAnyReference(imageRef)
	if err != nil {
		return "", err
	}
	canonical, ok := parsed.(reference.Canonical)
	if !ok {
		return "", fmt.Errorf("%s is not a canonical digest reference", imageRef)
	}
	return canonical.Digest().String(), nil
}

func (l *PodmanControllerLauncher) create(ctx context.Context, name string, c Cluster, image, containerName string) (string, error) {
	cfg := c.ControllerConfig()

	kc, err := kube.New(c.KubeConfig(), "")
	if err != nil {
		return "", fmt.Errorf("loading kubeconfig: %w", err)
	}
	if err := kc.WriteFlattened(c.KubeConfig()); err != nil {
		return "", fmt.Errorf("flattening kubeconfig: %w", err)
	}

	env := map[string]string{
		"KIT_KUBECONFIG":     "/run/kit/kubeconfig",
		"KIT_CONTAINER_NAME": containerName,
	}
	mounts := []podman.Mount{
		{Source: c.KubeConfig(), Target: "/run/kit/kubeconfig", Options: "ro,z"},
	}

	if l.cacheDir != "" {
		env["KIT_CACHE_DIR"] = l.cacheDir
		mounts = append(mounts, podman.Mount{Source: l.cacheDir, Target: l.cacheDir, Options: "z"})
	}
	if l.collectionsDir != "" {
		env["KIT_COLLECTIONS_DIR"] = l.collectionsDir
		mounts = append(mounts, podman.Mount{Source: l.collectionsDir, Target: l.collectionsDir, Options: "ro,z"})
	}
	for _, dir := range []string{cfg.LogsDir, cfg.VolumesDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("creating directory %s: %w", dir, err)
		}
		mounts = append(mounts, podman.Mount{Source: dir, Target: dir, Options: "z"})
	}
	if cfg.APIServerHost != "" {
		env["KIT_API_SERVER"] = fmt.Sprintf("https://%s:%s", cfg.APIServerHost, cfg.APIServerPort)
	}
	if cfg.Provider == "kwok" {
		const socketTarget = "/run/podman/podman.sock"
		mounts = append(mounts, podman.Mount{Source: podmanSocketPath(), Target: socketTarget, Options: "z"})
		env["KIT_PODMAN_SOCKET"] = socketTarget
	}

	// Nonce unique to this launch attempt so that cleanupFailedRun can tell apart
	// a container we partially created from one an external process raced in.
	launchID := generateLaunchID()

	l.log.Info("launching controller container", "name", containerName, "image", image, "network", cfg.Network)

	runOpts := podman.RunOpts{
		Name:    containerName,
		Image:   image,
		Network: cfg.Network,
		Labels: map[string]string{
			"kit.role":      "controller",
			"kit.cluster":   name,
			"kit.launch-id": launchID,
		},
		Env:       env,
		Mounts:    mounts,
		Detach:    true,
		LogDriver: "journald",
		HealthCmd: "curl -fsS --max-time 2 http://127.0.0.1:8081/readyz",
	}
	if cfg.Provider == "kwok" {
		runOpts.SecurityOpt = []string{"label=disable"}
	}

	containerID, err := l.client.Run(ctx, runOpts)
	if err != nil {
		l.cleanupFailedRun(containerName, launchID)
		return "", fmt.Errorf("launching controller container %s: %w", containerName, err)
	}

	return containerID, nil
}

// cleanupFailedRun removes a container only if its kit.launch-id label matches
// the nonce of the current launch attempt. This ensures we never delete a
// container created by an external process that raced between our inspect and run.
func (l *PodmanControllerLauncher) cleanupFailedRun(containerName, launchID string) {
	info, err := l.client.ContainerInspect(context.Background(), containerName)
	if err != nil {
		l.log.Warn("cleanup: failed to inspect container after failed run", "name", containerName, "error", err)
		return
	}
	if info == nil {
		return
	}
	if info.Labels["kit.launch-id"] != launchID {
		l.log.Warn("run failed but container was not created by this launch attempt; not removing", "name", containerName)
		return
	}
	if err := l.client.Remove(context.Background(), info.ID, true); err != nil {
		l.log.Warn("cleanup: failed to remove container after failed run", "name", containerName, "id", info.ID, "error", err)
	}
}

func generateLaunchID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

const controllerHealthTimeout = 2 * time.Minute

func (l *PodmanControllerLauncher) WaitHealthy(ctx context.Context, name string) error {
	containerName := naming.ContainerName(name)
	ctx, cancel := context.WithTimeout(ctx, controllerHealthTimeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := l.client.HealthCheckRun(ctx, containerName); err == nil {
			l.log.Info("controller healthy", "name", containerName)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("controller %s did not become healthy within %s", containerName, controllerHealthTimeout)
		case <-ticker.C:
		}
	}
}

func podmanSocketPath() string {
	return fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid())
}
