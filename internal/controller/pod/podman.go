package pod

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/pullsecret"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type podmanRunner struct {
	podmanClient  podman.Client
	clients       *kube.Clients
	logsDir       string
	volumesDir    string
	pullSecret    *pullsecret.PullSecret
	network       string
	apiServerHost string
	apiServerPort string
	changes       chan ContainerStateChange
	log           *slog.Logger
	wg            sync.WaitGroup
	mu            sync.Mutex
	pods          map[string]podmanPod // k8s pod UID -> podman pod state
	running       map[string]struct{}  // processKey -> running
	stopped       atomic.Bool
}

type podmanPod struct {
	id      string
	aliases []string
}

type PodmanRunnerOpts struct {
	PodmanClient  podman.Client
	Clients       *kube.Clients
	PullSecret    *pullsecret.PullSecret
	LogsDir       string
	VolumesDir    string
	Network       string
	APIServerHost string
	APIServerPort string
}

func NewPodmanRunner(ctx context.Context, opts PodmanRunnerOpts, log *slog.Logger) (PodRunner, error) {
	r := &podmanRunner{
		podmanClient:  opts.PodmanClient,
		clients:       opts.Clients,
		logsDir:       opts.LogsDir,
		volumesDir:    opts.VolumesDir,
		pullSecret:    opts.PullSecret,
		network:       opts.Network,
		apiServerHost: opts.APIServerHost,
		apiServerPort: opts.APIServerPort,
		changes:       make(chan ContainerStateChange, 100),
		log:           log,
		pods:          make(map[string]podmanPod),
		running:       make(map[string]struct{}),
	}
	r.recoverPods(ctx)
	return r, nil
}

func (r *podmanRunner) recoverPods(ctx context.Context) {
	pods, err := r.podmanClient.PodList(ctx, map[string]string{"network": r.network})
	if err != nil {
		return
	}
	for _, p := range pods {
		uid := p.Labels["kit.pod-uid"]
		if uid == "" {
			continue
		}
		var aliases []string
		if raw := p.Labels["kit.aliases"]; raw != "" {
			aliases = strings.Split(raw, ",")
		}
		r.pods[uid] = podmanPod{id: p.ID, aliases: aliases}
		r.log.Info("recovered podman pod", "uid", uid, "podID", p.ID)
	}
}

func (r *podmanRunner) RunPod(ctx context.Context, pod *corev1.Pod) error {
	if r.stopped.Load() {
		return nil
	}

	uid := string(pod.UID)

	podID, err := r.ensurePod(ctx, pod)
	if err != nil {
		return err
	}

	var firstErr error
	for _, container := range pod.Spec.Containers {
		key := processKey(uid, container.Name)
		r.mu.Lock()
		_, exists := r.running[key]
		r.mu.Unlock()
		if exists {
			continue
		}

		if err := r.startContainer(ctx, pod, podID, container); err != nil {
			r.log.Error("failed to start container", "pod", pod.Name, "namespace", pod.Namespace, "container", container.Name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (r *podmanRunner) StopPod(uid string) {
	r.mu.Lock()
	p, ok := r.pods[uid]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.pods, uid)
	for key := range r.running {
		if strings.HasPrefix(key, uid+"/") {
			delete(r.running, key)
		}
	}
	r.mu.Unlock()

	r.log.Info("stopping podman pod", "podID", p.id)
	_ = r.podmanClient.PodStop(context.Background(), p.id, 10)
	_ = r.podmanClient.PodRemove(context.Background(), p.id, true)
}

func (r *podmanRunner) Teardown() {
	r.log.Info("graceful shutdown of pod runner")
	r.stopped.Store(true)
	close(r.changes)

	r.mu.Lock()
	pods := make(map[string]podmanPod, len(r.pods))
	for k, v := range r.pods {
		pods[k] = v
	}
	r.pods = make(map[string]podmanPod)
	r.running = make(map[string]struct{})
	r.mu.Unlock()

	for _, p := range pods {
		r.log.Info("stopping podman pod", "podID", p.id)
		_ = r.podmanClient.PodStop(context.Background(), p.id, 10)
		_ = r.podmanClient.PodRemove(context.Background(), p.id, true)
	}

	r.wg.Wait()
}

func (r *podmanRunner) Changes() <-chan ContainerStateChange {
	return r.changes
}

func (r *podmanRunner) LogPath(namespace, pod, container string) string {
	return containerLogPath(r.logsDir, namespace, pod, container)
}

func (r *podmanRunner) ensurePod(ctx context.Context, pod *corev1.Pod) (string, error) {
	uid := string(pod.UID)

	aliases := r.serviceAliases(ctx, pod)

	r.mu.Lock()
	if existing, ok := r.pods[uid]; ok {
		if !hasNewAliases(existing.aliases, aliases) {
			r.mu.Unlock()
			return existing.id, nil
		}
		r.mu.Unlock()
		r.log.Info("recreating pod for new service aliases", "pod", pod.Name, "namespace", pod.Namespace)
		r.StopPod(uid)
	} else {
		r.mu.Unlock()
	}

	name := "kit-" + uid[:12]
	id, err := r.podmanClient.PodCreate(ctx, podman.PodCreateOpts{
		Name:    name,
		Network: r.network,
		Labels: map[string]string{
			"kit.pod-uid": uid,
			"kit.aliases": strings.Join(aliases, ","),
		},
		NetworkAliases: aliases,
	})
	if err != nil {
		return "", fmt.Errorf("creating podman pod: %w", err)
	}

	r.mu.Lock()
	r.pods[uid] = podmanPod{id: id, aliases: aliases}
	r.mu.Unlock()

	if len(aliases) > 0 {
		r.log.Info("registered service DNS aliases", "pod", pod.Name, "namespace", pod.Namespace, "aliases", aliases)
	}
	return id, nil
}

func hasNewAliases(current, wanted []string) bool {
	if len(wanted) <= len(current) {
		return false
	}
	have := make(map[string]struct{}, len(current))
	for _, a := range current {
		have[a] = struct{}{}
	}
	for _, a := range wanted {
		if _, ok := have[a]; !ok {
			return true
		}
	}
	return false
}

func (r *podmanRunner) serviceAliases(ctx context.Context, pod *corev1.Pod) []string {
	svcs, err := r.clients.Clientset.CoreV1().Services(pod.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		r.log.Warn("failed to list services for alias resolution", "namespace", pod.Namespace, "error", err)
		return nil
	}

	var aliases []string
	for _, svc := range svcs.Items {
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		if !selectorMatchesLabels(svc.Spec.Selector, pod.Labels) {
			continue
		}
		base := svc.Name + "." + pod.Namespace
		aliases = append(aliases,
			base+".svc.cluster.local",
			base+".svc",
			base,
		)
	}
	return aliases
}

func selectorMatchesLabels(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (r *podmanRunner) startContainer(ctx context.Context, pod *corev1.Pod, podID string, container corev1.Container) error {
	uid := string(pod.UID)
	key := processKey(uid, container.Name)
	containerName := "kit-" + uid[:12] + "-" + container.Name

	onWait := volumeEventEmitter(r.clients.Clientset, pod, r.log)
	mounts, err := waitForVolumes(ctx, r.clients.Clientset, r.volumesDir, pod, container, r.log, onWait)
	if err != nil {
		var notReady *VolumeNotReadyError
		if errors.As(err, &notReady) {
			return fmt.Errorf("timed out waiting for volumes for %s/%s: %w", pod.Name, container.Name, err)
		}
		r.log.Warn("failed to materialize some volumes", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
	}

	saToken, err := materializeSAToken(ctx, r.clients.Clientset, r.clients.REST, r.volumesDir, pod)
	if err != nil {
		return fmt.Errorf("materializing SA token: %w", err)
	}

	logPath := containerLogPath(r.logsDir, pod.Namespace, pod.Name, container.Name)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}

	const saMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

	env := make(map[string]string)
	for _, e := range container.Env {
		if e.ValueFrom != nil {
			continue
		}
		env[e.Name] = e.Value
	}
	apiHost := saToken.Host
	apiPort := saToken.Port
	if r.apiServerHost != "" {
		apiHost = r.apiServerHost
		apiPort = r.apiServerPort
	}
	env["POD_NAME"] = pod.Name
	env["POD_NAMESPACE"] = pod.Namespace
	env["KUBERNETES_SERVICE_HOST"] = apiHost
	env["KUBERNETES_SERVICE_PORT"] = apiPort

	runMounts := []podman.Mount{
		{Source: saToken.MountDir, Target: saMountPath, Options: "ro,z"},
	}
	for mountPath, localPath := range mounts {
		runMounts = append(runMounts, podman.Mount{Source: localPath, Target: mountPath, Options: "z"})
	}

	var containerID string
	err = r.pullSecret.WithAuthFile(func(authFile string) error {
		var runErr error
		containerID, runErr = r.podmanClient.Run(ctx, podman.RunOpts{
			Name:      containerName,
			Image:     container.Image,
			Pod:       podID,
			Detach:    true,
			Replace:   true,
			LogDriver: "k8s-file",
			LogOpts:   map[string]string{"path": logPath},
			Env:       env,
			Mounts:    runMounts,
			AuthFile:  authFile,
			Entrypoint: container.Command,
			Args:       container.Args,
		})
		return runErr
	})
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.running[key] = struct{}{}
	r.mu.Unlock()

	shortID := containerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	r.log.Info("started container", "pod", pod.Name, "namespace", pod.Namespace, "container", container.Name, "containerID", shortID)

	r.changes <- ContainerStateChange{
		PodUID:    uid,
		PodName:   pod.Name,
		Namespace: pod.Namespace,
		Container: container.Name,
		State:     ContainerStarted,
	}

	r.wg.Add(1)
	go r.waitContainer(uid, pod.Name, pod.Namespace, container.Name, containerID)

	return nil
}

func (r *podmanRunner) waitContainer(uid, podName, namespace, container, containerID string) {
	defer r.wg.Done()
	key := processKey(uid, container)

	// TODO: use a cancellable context so Ctrl-C / SIGTERM unblocks this
	exitCode, err := r.podmanClient.Wait(context.Background(), containerID)

	r.mu.Lock()
	delete(r.running, key)
	r.mu.Unlock()

	if err != nil {
		exitCode = 1
	}

	var exitErr error
	if exitCode != 0 {
		exitErr = fmt.Errorf("container %s exited with code %d", container, exitCode)
	}

	r.log.Info("container exited", "pod", podName, "namespace", namespace, "container", container, "exitCode", exitCode)

	if r.stopped.Load() {
		return
	}

	r.changes <- ContainerStateChange{
		PodUID:    uid,
		PodName:   podName,
		Namespace: namespace,
		Container: container,
		State:     ContainerExited,
		ExitCode:  int32(exitCode),
		Err:       exitErr,
	}
}
