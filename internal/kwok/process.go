package kwok

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type processRunner struct {
	kubeconfig string
	clientset  kubernetes.Interface
	registry   *image.Registry
	logsDir    string
	volumesDir string
	log        *slog.Logger
	procMgr    *processManager
	changes    chan<- ContainerStateChange
}

func newProcessRunner(kubeconfig string, registry *image.Registry, logsDir, volumesDir string, changes chan<- ContainerStateChange, log *slog.Logger) (*processRunner, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}

	r := &processRunner{
		kubeconfig: kubeconfig,
		clientset:  clientset,
		registry:   registry,
		logsDir:    logsDir,
		volumesDir: volumesDir,
		log:        log,
		changes:    changes,
	}
	r.procMgr = newProcessManager(log, r.onProcessExit)
	return r, nil
}

func (r *processRunner) RunPod(ctx context.Context, pod *corev1.Pod) error {
	uid := string(pod.UID)
	var firstErr error
	for _, container := range pod.Spec.Containers {
		key := processKey(uid, container.Name)
		r.procMgr.mu.Lock()
		_, exists := r.procMgr.procs[key]
		r.procMgr.mu.Unlock()
		if exists {
			continue
		}

		if err := r.startContainer(ctx, pod, container); err != nil {
			r.log.Error("failed to start container", "pod", pod.Name, "namespace", pod.Namespace, "container", container.Name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (r *processRunner) StopPod(uid string) {
	r.procMgr.mu.Lock()
	var keys []string
	for key, proc := range r.procMgr.procs {
		if proc.podUID == uid {
			keys = append(keys, key)
		}
	}
	r.procMgr.mu.Unlock()

	for _, key := range keys {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) == 2 {
			r.procMgr.Stop(parts[0], parts[1])
		}
	}
}

func (r *processRunner) StopAll() {
	r.procMgr.StopAll()
}

func (r *processRunner) LogPath(namespace, pod, container string) string {
	return containerLogPath(r.logsDir, namespace, pod, container)
}

func (r *processRunner) onProcessExit(uid, podName, namespace, container string, err error) {
	exitCode := exitCodeFromErr(err)
	r.changes <- ContainerStateChange{
		PodUID:    uid,
		PodName:   podName,
		Namespace: namespace,
		Container: container,
		State:     ContainerExited,
		ExitCode:  exitCode,
		Err:       err,
	}
}

func (r *processRunner) startContainer(ctx context.Context, pod *corev1.Pod, container corev1.Container) error {
	command := container.Command
	args := container.Args

	if len(command) == 0 {
		imgCfg, err := r.registry.ImageConfig(container.Image)
		if err != nil {
			return fmt.Errorf("getting image config for %s: %w", container.Image, err)
		}
		command = imgCfg.Entrypoint
		if len(args) == 0 {
			args = imgCfg.Cmd
		}
	}

	if len(command) == 0 {
		return fmt.Errorf("no entrypoint or command for container %s", container.Name)
	}

	binaryPath := command[0]
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(strings.TrimPrefix(binaryPath, "/")) + "$")
	extracted, err := r.registry.Extract(container.Image, []image.Matcher{{Pattern: pattern}})
	if err != nil {
		return fmt.Errorf("extracting binary %s: %w", binaryPath, err)
	}

	localBinary, ok := extracted[pattern.String()]
	if !ok {
		return fmt.Errorf("binary %s not found in image %s", binaryPath, container.Image)
	}

	ldLibraryPath := r.resolveSharedLibs(container.Image, localBinary)

	pathRewrites, err := r.materializeVolumes(ctx, pod, container)
	if err != nil {
		r.log.Warn("failed to materialize some volumes", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
	}

	var fullArgs []string
	if len(command) > 1 {
		fullArgs = append(fullArgs, command[1:]...)
	}
	fullArgs = append(fullArgs, args...)
	fullArgs = rewritePaths(fullArgs, pathRewrites)

	env := r.buildEnv(pod, container)
	env = rewritePaths(env, pathRewrites)
	if ldLibraryPath != "" {
		env = append(env, "LD_LIBRARY_PATH="+ldLibraryPath)
	}

	if err := r.procMgr.Start(ctx, processOpts{
		PodUID:    string(pod.UID),
		PodName:   pod.Name,
		Namespace: pod.Namespace,
		Container: container.Name,
		Binary:    localBinary,
		Args:      fullArgs,
		Env:       env,
		LogsDir:   r.logsDir,
	}); err != nil {
		return err
	}

	r.changes <- ContainerStateChange{
		PodUID:    string(pod.UID),
		PodName:   pod.Name,
		Namespace: pod.Namespace,
		Container: container.Name,
		State:     ContainerStarted,
	}

	return nil
}

func (r *processRunner) buildEnv(pod *corev1.Pod, container corev1.Container) []string {
	var env []string
	for _, e := range container.Env {
		if e.ValueFrom != nil {
			continue
		}
		env = append(env, fmt.Sprintf("%s=%s", e.Name, e.Value))
	}

	env = append(env,
		"KUBECONFIG="+r.kubeconfig,
		"POD_NAME="+pod.Name,
		"POD_NAMESPACE="+pod.Namespace,
	)

	return env
}

func (r *processRunner) materializeVolumes(ctx context.Context, pod *corev1.Pod, container corev1.Container) (map[string]string, error) {
	volumeMap := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		volumeMap[v.Name] = v
	}

	rewrites := make(map[string]string)
	var firstErr error
	for _, vm := range container.VolumeMounts {
		vol, ok := volumeMap[vm.Name]
		if !ok {
			continue
		}

		localDir := filepath.Join(r.volumesDir, pod.Namespace, pod.Name, vm.Name)
		if err := os.MkdirAll(localDir, 0o755); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		var data map[string][]byte
		switch {
		case vol.ConfigMap != nil:
			cm, err := r.clientset.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, vol.ConfigMap.Name, metav1.GetOptions{})
			if err != nil {
				r.log.Warn("failed to get configmap for volume", "configmap", vol.ConfigMap.Name, "volume", vm.Name, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			data = make(map[string][]byte, len(cm.Data)+len(cm.BinaryData))
			for k, v := range cm.Data {
				data[k] = []byte(v)
			}
			for k, v := range cm.BinaryData {
				data[k] = v
			}
		case vol.Secret != nil:
			secret, err := r.clientset.CoreV1().Secrets(pod.Namespace).Get(ctx, vol.Secret.SecretName, metav1.GetOptions{})
			if err != nil {
				r.log.Warn("failed to get secret for volume", "secret", vol.Secret.SecretName, "volume", vm.Name, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			data = secret.Data
		default:
			continue
		}

		for filename, content := range data {
			if err := os.WriteFile(filepath.Join(localDir, filename), content, 0o644); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		rewrites[vm.MountPath] = localDir
	}

	return rewrites, firstErr
}

func (r *processRunner) resolveSharedLibs(imageRef, binaryPath string) string {
	out, _ := exec.Command("ldd", binaryPath).CombinedOutput()

	var missingLibs []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "not found") {
			parts := strings.Fields(strings.TrimSpace(line))
			if len(parts) > 0 {
				missingLibs = append(missingLibs, parts[0])
			}
		}
	}

	if len(missingLibs) == 0 {
		return ""
	}

	r.log.Info("resolving missing shared libraries", "binary", binaryPath, "libs", missingLibs)

	dirSet := make(map[string]struct{})
	for _, lib := range missingLibs {
		pattern := regexp.MustCompile(`(usr/)?lib64/` + regexp.QuoteMeta(lib) + `(\.\d+)*$`)
		extracted, err := r.registry.Extract(imageRef, []image.Matcher{{Pattern: pattern}})
		if err != nil {
			r.log.Warn("shared library not found in image", "lib", lib, "error", err)
			continue
		}

		localPath := extracted[pattern.String()]
		dir := filepath.Dir(localPath)
		dirSet[dir] = struct{}{}

		if filepath.Base(localPath) != lib {
			os.Symlink(filepath.Base(localPath), filepath.Join(dir, lib))
		}
	}

	var dirs []string
	for dir := range dirSet {
		dirs = append(dirs, dir)
	}
	return strings.Join(dirs, ":")
}

func rewritePaths(values []string, rewrites map[string]string) []string {
	if len(rewrites) == 0 {
		return values
	}
	out := make([]string, len(values))
	for i, v := range values {
		for mountPath, localPath := range rewrites {
			v = strings.ReplaceAll(v, mountPath, localPath)
		}
		out[i] = v
	}
	return out
}

func exitCodeFromErr(err error) int32 {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return int32(exitErr.ExitCode())
	}
	return 1
}

// --- process manager (internal) ---

type processOpts struct {
	PodUID    string
	PodName   string
	Namespace string
	Container string
	Binary    string
	Args      []string
	Env       []string
	LogsDir   string
}

type managedProcess struct {
	cmd       *exec.Cmd
	logFile   *os.File
	cancel    context.CancelFunc
	podUID    string
	podName   string
	namespace string
	container string
}

type processManager struct {
	mu      sync.Mutex
	procs   map[string]*managedProcess
	onExit  func(uid, podName, namespace, container string, err error)
	log     *slog.Logger
}

func newProcessManager(log *slog.Logger, onExit func(uid, podName, namespace, container string, err error)) *processManager {
	return &processManager{
		procs:  make(map[string]*managedProcess),
		onExit: onExit,
		log:    log,
	}
}

func processKey(podUID, container string) string {
	return podUID + "/" + container
}

func (pm *processManager) Start(ctx context.Context, opts processOpts) error {
	key := processKey(opts.PodUID, opts.Container)

	pm.mu.Lock()
	if _, exists := pm.procs[key]; exists {
		pm.mu.Unlock()
		return nil
	}
	pm.mu.Unlock()

	logDir := filepath.Join(opts.LogsDir, opts.Namespace, opts.PodName)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}

	logPath := filepath.Join(logDir, opts.Container+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}

	stdoutWriter := newCRIWriter(logFile, "stdout")
	stderrWriter := newCRIWriter(logFile, "stderr")

	procCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(procCtx, opts.Binary, opts.Args...)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	cmd.Env = opts.Env

	if err := cmd.Start(); err != nil {
		cancel()
		logFile.Close()
		return fmt.Errorf("starting %s: %w", opts.Binary, err)
	}

	proc := &managedProcess{
		cmd:       cmd,
		logFile:   logFile,
		cancel:    cancel,
		podUID:    opts.PodUID,
		podName:   opts.PodName,
		namespace: opts.Namespace,
		container: opts.Container,
	}

	pm.mu.Lock()
	pm.procs[key] = proc
	pm.mu.Unlock()

	pm.log.Info("started process", "pod", opts.PodName, "namespace", opts.Namespace, "container", opts.Container, "pid", cmd.Process.Pid, "binary", opts.Binary)

	go func() {
		err := cmd.Wait()
		logFile.Close()
		pm.mu.Lock()
		delete(pm.procs, key)
		pm.mu.Unlock()

		pm.log.Info("process exited", "pod", opts.PodName, "namespace", opts.Namespace, "container", opts.Container, "error", err)
		pm.onExit(opts.PodUID, opts.PodName, opts.Namespace, opts.Container, err)
	}()

	return nil
}

func (pm *processManager) Stop(podUID, container string) {
	key := processKey(podUID, container)

	pm.mu.Lock()
	proc, exists := pm.procs[key]
	if !exists {
		pm.mu.Unlock()
		return
	}
	delete(pm.procs, key)
	pm.mu.Unlock()

	pm.log.Info("stopping process", "pod", proc.podName, "namespace", proc.namespace, "container", proc.container)
	proc.cancel()
}

func (pm *processManager) StopAll() {
	pm.mu.Lock()
	procs := make(map[string]*managedProcess, len(pm.procs))
	for k, v := range pm.procs {
		procs[k] = v
	}
	pm.procs = make(map[string]*managedProcess)
	pm.mu.Unlock()

	for _, proc := range procs {
		pm.log.Info("stopping process", "pod", proc.podName, "namespace", proc.namespace, "container", proc.container)
		proc.cancel()
	}
}

func containerLogPath(logsDir, namespace, pod, container string) string {
	return filepath.Join(logsDir, namespace, pod, container+".log")
}

// --- CRI log writer ---

type criWriter struct {
	out    *os.File
	stream string
	buf    []byte
}

func newCRIWriter(out *os.File, stream string) *criWriter {
	return &criWriter{out: out, stream: stream}
}

func (w *criWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	written := len(p)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := w.buf[:idx]
		w.buf = w.buf[idx+1:]
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		fmt.Fprintf(w.out, "%s %s F %s\n", ts, w.stream, line)
	}
	return written, nil
}
