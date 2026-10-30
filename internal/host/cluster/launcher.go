package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
)

type PodmanControllerLauncher struct {
	client         podman.Client
	image          string
	cacheDir       string
	collectionsDir string
	log            *slog.Logger
}

func NewPodmanControllerLauncher(client podman.Client, image, cacheDir, collectionsDir string, log *slog.Logger) *PodmanControllerLauncher {
	return &PodmanControllerLauncher{client: client, image: image, cacheDir: cacheDir, collectionsDir: collectionsDir, log: log}
}

func (l *PodmanControllerLauncher) Launch(ctx context.Context, name string, c Cluster) (func(), error) {
	containerName := "kit-controller-" + name
	cfg := c.ControllerConfig()

	kc, err := kube.New(c.KubeConfig(), "")
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	if err := kc.WriteFlattened(c.KubeConfig()); err != nil {
		return nil, fmt.Errorf("flattening kubeconfig: %w", err)
	}

	env := map[string]string{
		"KIT_KUBECONFIG": "/run/kit/kubeconfig",
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
			return nil, fmt.Errorf("creating directory %s: %w", dir, err)
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

	l.log.Info("launching controller container", "name", containerName, "image", l.image, "network", cfg.Network)

	runOpts := podman.RunOpts{
		Name:    containerName,
		Image:   l.image,
		Network: cfg.Network,
		Labels: map[string]string{
			"kit.role":    "controller",
			"kit.cluster": name,
		},
		Env:       env,
		Mounts:    mounts,
		Detach:    true,
		Replace:   true,
		LogDriver: "journald",
	}
	if cfg.Provider == "kwok" {
		runOpts.SecurityOpt = []string{"label=disable"}
	}

	_, err = l.client.Run(ctx, runOpts)
	if err != nil {
		// podman may have created the container and attached it to the network
		// before the OCI runtime failed; remove it to avoid blocking further cluster cleanup.
		_ = l.client.Remove(context.Background(), containerName, true)
		return nil, fmt.Errorf("launching controller container %s: %w", containerName, err)
	}

	stop := func() {
		l.log.Info("stopping controller container", "name", containerName)
		_ = l.client.Stop(context.Background(), containerName, 10)
		_ = l.client.Remove(context.Background(), containerName, true)
	}

	return stop, nil
}

func podmanSocketPath() string {
	return fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid())
}
