package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/imageref"
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

func (l *PodmanControllerLauncher) Launch(ctx context.Context, name string, c Cluster, image imageref.Resolved) (func(), error) {
	containerName := naming.ContainerName(name)

	var containerID string

	existing, err := l.client.ContainerInspect(ctx, containerName)
	if err != nil {
		return nil, fmt.Errorf("inspecting controller container %s: %w", containerName, err)
	}
	if existing != nil {
		if err := l.handleExisting(existing, containerName, name, image.Digest()); err != nil {
			return nil, err
		}
		containerID = existing.ID
	} else {
		containerID, err = l.create(ctx, name, c, image.String(), containerName)
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

func (l *PodmanControllerLauncher) handleExisting(info *podman.ContainerInfo, containerName, clusterName, expectedDigest string) error {
	if info.Labels["kit.role"] != "controller" || info.Labels["kit.cluster"] != clusterName {
		return fmt.Errorf("container %s exists but belongs to a different cluster or role", containerName)
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
