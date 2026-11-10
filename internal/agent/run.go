package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/afero"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/download"
)

type RuntimeFactory func(name string) (Runtime, error)

func Run(ctx context.Context, log *slog.Logger, fs afero.Fs, cfg Config, runtimeFactory RuntimeFactory, kube KubeClient) (err error) {
	if kube == nil {
		return fmt.Errorf("kubernetes client is required")
	}

	bundleDir, err := afero.TempDir(fs, "", "kit-agent-*")
	if err != nil {
		return fmt.Errorf("creating temp directory: %w", err)
	}
	defer func() {
		cleanupErr := fs.RemoveAll(bundleDir)
		err = errors.Join(err, cleanupErr)
	}()

	log.Info("downloading bundle", "url", cfg.BundleURL, "digest", cfg.BundleDigest)
	if err := download.Download(ctx, http.DefaultClient, fs, cfg.BundleURL, cfg.BundleDigest, bundleDir); err != nil {
		return fmt.Errorf("downloading bundle: %w", err)
	}

	manifest, err := readManifest(fs, bundleDir)
	if err != nil {
		return err
	}

	values, err := readValues(fs, bundleDir)
	if err != nil {
		return err
	}

	pod := readPodIdentity(fs)

	req := AgentRequest{
		BundleDir:  bundleDir,
		Entrypoint: manifest.Entrypoint,
		Lifecycle:  Lifecycle(manifest.Lifecycle),
		Values:     values,
		Pod:        pod,
	}

	rt, err := runtimeFactory(manifest.Runtime)
	if err != nil {
		return err
	}

	h := newHost(log, kube)

	log.Info("agent ready",
		"runtime", manifest.Runtime,
		"lifecycle", manifest.Lifecycle,
		"entrypoint", req.Entrypoint,
		"pod", req.Pod.Name,
		"namespace", req.Pod.Namespace,
	)

	result, err := rt.Run(ctx, req, h)
	if err != nil {
		return fmt.Errorf("runtime %s: %w", rt.Name(), err)
	}

	log.Info("agent finished", "exitCode", result.ExitCode, "reason", result.Reason)

	if result.ExitCode != 0 {
		return fmt.Errorf("script exited with code %d: %s", result.ExitCode, result.Reason)
	}

	return nil
}

func readManifest(fs afero.Fs, bundleDir string) (*bundle.Manifest, error) {
	data, err := afero.ReadFile(fs, filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("reading manifest.json: %w", err)
	}
	var m bundle.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest.json: %w", err)
	}
	return &m, nil
}

func readValues(fs afero.Fs, bundleDir string) (map[string]any, error) {
	data, err := afero.ReadFile(fs, filepath.Join(bundleDir, "values.json"))
	if err != nil {
		return nil, fmt.Errorf("reading values.json: %w", err)
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parsing values.json: %w", err)
	}
	return values, nil
}

func readPodIdentity(fs afero.Fs) PodReference {
	namespace, _ := afero.ReadFile(fs, "/var/run/secrets/kubernetes.io/serviceaccount/namespace")

	podName := os.Getenv("KIT_POD_NAME")
	if podName == "" {
		podName, _ = os.Hostname()
	}

	return PodReference{
		Name:      podName,
		Namespace: string(namespace),
		UID:       types.UID(os.Getenv("KIT_POD_UID")),
		NodeName:  os.Getenv("KIT_NODE_NAME"),
	}
}
