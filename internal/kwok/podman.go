package kwok

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type podmanRunner struct {
	kubeconfig    string
	clientset     kubernetes.Interface
	logsDir       string
	volumesDir    string
	authFile      string
	network       string
	apiServerHost string
	apiServerPort string
	changes       chan<- ContainerStateChange
	log           *slog.Logger
	mu            sync.Mutex
	pods          map[string]podmanPod // k8s pod UID -> podman pod state
	running       map[string]struct{}  // processKey -> running
}

type podmanPod struct {
	id      string
	aliases []string
}

type podmanRunnerOpts struct {
	Kubeconfig    string
	AuthFile      string
	LogsDir       string
	VolumesDir    string
	Network       string
	APIServerHost string
	APIServerPort string
}

func newPodmanRunner(opts podmanRunnerOpts, changes chan<- ContainerStateChange, log *slog.Logger) (*podmanRunner, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", opts.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}

	return &podmanRunner{
		kubeconfig:    opts.Kubeconfig,
		clientset:     clientset,
		logsDir:       opts.LogsDir,
		volumesDir:    opts.VolumesDir,
		authFile:      opts.AuthFile,
		network:       opts.Network,
		apiServerHost: opts.APIServerHost,
		apiServerPort: opts.APIServerPort,
		changes:       changes,
		log:           log,
		pods:          make(map[string]podmanPod),
		running:       make(map[string]struct{}),
	}, nil
}

func (r *podmanRunner) RunPod(ctx context.Context, pod *corev1.Pod) error {
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
	_ = exec.Command("podman", "pod", "stop", "-t", "10", p.id).Run()
	_ = exec.Command("podman", "pod", "rm", "-f", p.id).Run()
}

func (r *podmanRunner) StopAll() {
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
		_ = exec.Command("podman", "pod", "stop", "-t", "10", p.id).Run()
		_ = exec.Command("podman", "pod", "rm", "-f", p.id).Run()
	}
}

func (r *podmanRunner) LogPath(namespace, pod, container string) string {
	return containerLogPath(r.logsDir, namespace, pod, container)
}

// ensurePod creates a podman pod for the given K8s pod. It also resolves
// Service DNS by adding --network-alias entries: for each Service in the
// pod's namespace whose selector matches the pod's labels, aliases like
// <svc>.<ns>.svc.cluster.local are added so that other pods on the same
// podman network can reach this pod via standard K8s service DNS names.
func (r *podmanRunner) ensurePod(ctx context.Context, pod *corev1.Pod) (string, error) {
	uid := string(pod.UID)

	// Look up Services whose selector matches this pod's labels to register
	// DNS aliases via podman's aardvark-dns resolver. This replaces the need
	// for CoreDNS or kube-proxy in the simulated cluster.
	aliases := r.serviceAliases(ctx, pod)

	r.mu.Lock()
	if existing, ok := r.pods[uid]; ok {
		// If new Services appeared since the pod was created, recreate
		// the podman pod so aardvark-dns picks up the new aliases.
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
	args := []string{"pod", "create", "--name", name, "--network", r.network}
	for _, alias := range aliases {
		args = append(args, "--network-alias", alias)
	}

	out, err := exec.Command("podman", args...).Output()
	if err != nil {
		return "", fmt.Errorf("creating podman pod: %w", err)
	}

	id := strings.TrimSpace(string(out))
	r.mu.Lock()
	r.pods[uid] = podmanPod{id: id, aliases: aliases}
	r.mu.Unlock()

	if len(aliases) > 0 {
		r.log.Info("registered service DNS aliases", "pod", pod.Name, "namespace", pod.Namespace, "aliases", aliases)
	}
	return id, nil
}

// hasNewAliases returns true if wanted contains aliases not present in current.
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

// serviceAliases returns the set of DNS names this pod should be reachable
// by, based on Services whose selector matches the pod's labels. Each
// matching Service produces three aliases mirroring K8s DNS conventions:
//   - <svc>.<ns>.svc.cluster.local
//   - <svc>.<ns>.svc
//   - <svc>.<ns>
func (r *podmanRunner) serviceAliases(ctx context.Context, pod *corev1.Pod) []string {
	svcs, err := r.clientset.CoreV1().Services(pod.Namespace).List(ctx, metav1.ListOptions{})
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

// selectorMatchesLabels returns true if every key-value pair in the selector
// is present in the labels map (i.e. the selector is a subset of the labels).
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

	onWait := volumeEventEmitter(r.clientset, pod, r.log)
	mounts, err := waitForVolumes(ctx, r.clientset, r.volumesDir, pod, container, r.log, onWait)
	if err != nil {
		var notReady *VolumeNotReadyError
		if errors.As(err, &notReady) {
			return fmt.Errorf("timed out waiting for volumes for %s/%s: %w", pod.Name, container.Name, err)
		}
		r.log.Warn("failed to materialize some volumes", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
	}

	saToken, err := materializeSAToken(ctx, r.clientset, r.kubeconfig, r.volumesDir, pod)
	if err != nil {
		return fmt.Errorf("materializing SA token: %w", err)
	}

	logPath := containerLogPath(r.logsDir, pod.Namespace, pod.Name, container.Name)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}

	const saMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

	args := []string{
		"run", "-d",
		"--pod", podID,
		"--name", containerName,
		"--replace",
		"--log-driver", "k8s-file",
		"--log-opt", "path=" + logPath,
	}

	for _, e := range container.Env {
		if e.ValueFrom != nil {
			continue
		}
		args = append(args, "-e", e.Name+"="+e.Value)
	}
	apiHost := saToken.Host
	apiPort := saToken.Port
	if r.apiServerHost != "" {
		apiHost = r.apiServerHost
		apiPort = r.apiServerPort
	}
	args = append(args,
		"-e", "POD_NAME="+pod.Name,
		"-e", "POD_NAMESPACE="+pod.Namespace,
		"-e", "KUBERNETES_SERVICE_HOST="+apiHost,
		"-e", "KUBERNETES_SERVICE_PORT="+apiPort,
	)

	args = append(args, "-v", saToken.MountDir+":"+saMountPath+":ro,z")

	for mountPath, localPath := range mounts {
		args = append(args, "-v", localPath+":"+mountPath+":z")
	}

	if r.authFile != "" {
		args = append(args, "--authfile", r.authFile)
	}

	if len(container.Command) > 0 {
		args = append(args, "--entrypoint", container.Command[0])
	}

	args = append(args, container.Image)

	if len(container.Command) > 1 {
		args = append(args, container.Command[1:]...)
	}
	if len(container.Args) > 0 {
		args = append(args, container.Args...)
	}

	out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting container %s: %s: %w", container.Name, string(out), err)
	}

	containerID := strings.TrimSpace(string(out))

	r.mu.Lock()
	r.running[key] = struct{}{}
	r.mu.Unlock()

	r.log.Info("started container", "pod", pod.Name, "namespace", pod.Namespace, "container", container.Name, "containerID", containerID[:12])

	r.changes <- ContainerStateChange{
		PodUID:    uid,
		PodName:   pod.Name,
		Namespace: pod.Namespace,
		Container: container.Name,
		State:     ContainerStarted,
	}

	go r.waitContainer(uid, pod.Name, pod.Namespace, container.Name, containerID)

	return nil
}

func (r *podmanRunner) waitContainer(uid, podName, namespace, container, containerID string) {
	key := processKey(uid, container)

	out, err := exec.Command("podman", "wait", containerID).Output()

	r.mu.Lock()
	delete(r.running, key)
	r.mu.Unlock()

	var exitCode int32
	if err == nil {
		code, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		exitCode = int32(code)
	} else {
		exitCode = 1
	}

	var exitErr error
	if exitCode != 0 {
		exitErr = fmt.Errorf("container %s exited with code %d", container, exitCode)
	}

	r.log.Info("container exited", "pod", podName, "namespace", namespace, "container", container, "exitCode", exitCode)

	r.changes <- ContainerStateChange{
		PodUID:    uid,
		PodName:   podName,
		Namespace: namespace,
		Container: container,
		State:     ContainerExited,
		ExitCode:  exitCode,
		Err:       exitErr,
	}
}
