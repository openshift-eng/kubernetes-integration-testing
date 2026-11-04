package pod

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/workload/override"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const podWorkers = 10

var logsGVR = schema.GroupVersionResource{
	Group:    "kwok.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "logs",
}

type podRoute int

const (
	podRouteIgnore podRoute = iota
	podRouteNoOp
	podRouteRunner
)

func routePod(pod *corev1.Pod) podRoute {
	if pod.Spec.NodeName == "" {
		return podRouteIgnore
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return podRouteIgnore
	}
	if override.IsNoOp(pod) {
		return podRouteNoOp
	}
	return podRouteRunner
}

type Controller struct {
	runner     PodRunner
	log        *slog.Logger
	clientset  kubernetes.Interface
	dynClient  dynamic.Interface
	queue      workqueue.TypedRateLimitingInterface[string]
	restartsMu sync.Mutex
	restarts   map[string]int
}

func NewController(clients *kube.Clients, runner PodRunner, log *slog.Logger) *Controller {
	return &Controller{
		runner:    runner,
		log:       log,
		clientset: clients.Clientset,
		dynClient: clients.Dynamic,
		queue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		restarts:  make(map[string]int),
	}
}

func (c *Controller) Run(ctx context.Context) error {
	defer c.queue.ShutDown()

	go c.processStateChanges(ctx, c.runner.Changes())

	factory := informers.NewSharedInformerFactory(c.clientset, 0)
	podInformer := factory.Core().V1().Pods().Informer()

	_, _ = podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueue,
		// Only enqueue updates for spec-level transitions this controller
		// cares about. Status-only updates (from kit itself, Kwok Stages,
		// or setPodWaiting) must not re-enqueue: they bypass the workqueue's
		// rate-limited backoff and cause a feedback loop. Future predicates
		// (e.g. podBecameTerminal for Podman cleanup) can be OR'd here.
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, okOld := oldObj.(*corev1.Pod)
			newPod, okNew := newObj.(*corev1.Pod)
			if !okOld || !okNew {
				return
			}
			if podBecameScheduled(oldPod, newPod) {
				c.enqueue(newObj)
			}
		},
		DeleteFunc: c.handleDelete,
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	for i := 0; i < podWorkers; i++ {
		go c.runWorker(ctx)
	}

	<-ctx.Done()
	return nil
}

func (c *Controller) handleDelete(obj interface{}) {
	pod := podFromDelete(obj)
	if pod == nil {
		return
	}

	if key, err := cache.MetaNamespaceKeyFunc(pod); err == nil {
		c.queue.Forget(key)
	}

	c.clearRestartCounts(pod)
	c.runner.StopPod(string(pod.UID))
}

func podFromDelete(obj interface{}) *corev1.Pod {
	if pod, ok := obj.(*corev1.Pod); ok {
		return pod
	}
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if pod, ok := tombstone.Obj.(*corev1.Pod); ok {
			return pod
		}
	}
	return nil
}

func (c *Controller) clearRestartCounts(pod *corev1.Pod) {
	uid := string(pod.UID)
	c.restartsMu.Lock()
	defer c.restartsMu.Unlock()
	for _, container := range pod.Spec.Containers {
		delete(c.restarts, processKey(uid, container.Name))
	}
}

func podBecameScheduled(oldPod, newPod *corev1.Pod) bool {
	return oldPod.Spec.NodeName == "" && newPod.Spec.NodeName != ""
}

func (c *Controller) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	c.queue.Add(key)
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)
	return true
}

func (c *Controller) reconcile(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}

	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	switch routePod(pod) {
	case podRouteNoOp:
		return c.reconcileNoOpStatus(ctx, pod)
	case podRouteRunner:
		return c.reconcileRunnerPod(ctx, pod)
	}
	return nil
}

func (c *Controller) reconcileRunnerPod(ctx context.Context, pod *corev1.Pod) error {
	if err := c.runner.RunPod(ctx, pod); err != nil {
		c.log.Error("failed to run pod", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
		c.setPodWaiting(pod, err)
		return err
	}
	return nil
}

func (c *Controller) reconcileNoOpStatus(ctx context.Context, pod *corev1.Pod) error {
	if !noOpStatusSeedable(pod) {
		return nil
	}

	applyNoOpStatus(pod, time.Now())

	_, err := c.clientset.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		c.log.Warn("failed to set no-op pod status", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
		return err
	}

	c.log.Info("set no-op pod status", "pod", pod.Name, "namespace", pod.Namespace)
	return nil
}

func applyNoOpStatus(pod *corev1.Pod, now time.Time) {
	t := metav1.NewTime(now)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: t},
		{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: t},
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: t},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: t},
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  override.NoOpContainerName,
		Image: override.NoOpImage,
		Ready: true,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: t},
		},
	}}
}

func (c *Controller) processStateChanges(ctx context.Context, changes <-chan ContainerStateChange) {
	for {
		select {
		case change, ok := <-changes:
			if !ok {
				return
			}
			c.onStateChange(ctx, change)
		case <-ctx.Done():
			return
		}
	}
}

func (c *Controller) onStateChange(ctx context.Context, change ContainerStateChange) {
	now := metav1.NewTime(time.Now())

	pod, err := c.clientset.CoreV1().Pods(change.Namespace).Get(ctx, change.PodName, metav1.GetOptions{})
	if err != nil {
		c.log.Warn("failed to get pod for status update", "pod", change.PodName, "namespace", change.Namespace, "error", err)
		return
	}

	switch change.State {
	case ContainerStarted:
		setContainerStatus(pod, change.Container, corev1.ContainerStatus{
			Name:  change.Container,
			Image: containerImage(pod, change.Container),
			Ready: true,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: now},
			},
		})
		computePodReadiness(pod, now)

		c.ensureLogsResource(ctx, pod, change.Container)

	case ContainerExited:
		restartPolicy := pod.Spec.RestartPolicy
		if restartPolicy == "" {
			restartPolicy = corev1.RestartPolicyAlways
		}

		shouldRestart := restartPolicy == corev1.RestartPolicyAlways ||
			(restartPolicy == corev1.RestartPolicyOnFailure && change.Err != nil)

		if shouldRestart {
			key := processKey(change.PodUID, change.Container)
			c.restartsMu.Lock()
			c.restarts[key]++
			count := c.restarts[key]
			c.restartsMu.Unlock()

			setContainerStatus(pod, change.Container, corev1.ContainerStatus{
				Name:         change.Container,
				Image:        containerImage(pod, change.Container),
				Ready:        false,
				RestartCount: int32(count),
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   change.ExitCode,
						FinishedAt: now,
					},
				},
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason: "CrashLoopBackOff",
					},
				},
			})
			computePodReadiness(pod, now)

			backoff := restartBackoff(count)
			c.log.Info("scheduling restart", "pod", change.PodName, "namespace", change.Namespace, "container", change.Container, "attempt", count, "backoff", backoff)

			queueKey, _ := cache.MetaNamespaceKeyFunc(pod)
			c.queue.AddAfter(queueKey, backoff)
		} else {
			setContainerStatus(pod, change.Container, corev1.ContainerStatus{
				Name:  change.Container,
				Image: containerImage(pod, change.Container),
				Ready: false,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   change.ExitCode,
						FinishedAt: now,
					},
				},
			})
			computePodReadiness(pod, now)
		}
	}

	if _, err := c.clientset.CoreV1().Pods(change.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		c.log.Warn("failed to update pod status", "pod", change.PodName, "namespace", change.Namespace, "state", change.State, "error", err)
	}
}

func containerImage(pod *corev1.Pod, name string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

func setContainerStatus(pod *corev1.Pod, name string, status corev1.ContainerStatus) {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			pod.Status.ContainerStatuses[i] = status
			return
		}
	}
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, status)
}

func computePodReadiness(pod *corev1.Pod, now metav1.Time) {
	allReady := true
	allTerminated := true
	anyRunning := false
	anyFailed := false

	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			allReady = false
		}
		if cs.State.Running != nil {
			anyRunning = true
		}
		if cs.State.Terminated == nil {
			allTerminated = false
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			anyFailed = true
		}
	}

	hasAllContainers := len(pod.Status.ContainerStatuses) == len(pod.Spec.Containers)
	ready := allReady && hasAllContainers

	readyStatus := corev1.ConditionFalse
	if ready {
		readyStatus = corev1.ConditionTrue
	}

	setCondition(pod, corev1.PodScheduled, corev1.ConditionTrue, now)
	setCondition(pod, corev1.PodInitialized, corev1.ConditionTrue, now)
	setCondition(pod, corev1.ContainersReady, readyStatus, now)
	setCondition(pod, corev1.PodReady, readyStatus, now)

	switch {
	case allTerminated && hasAllContainers:
		if anyFailed {
			pod.Status.Phase = corev1.PodFailed
		} else {
			pod.Status.Phase = corev1.PodSucceeded
		}
	case anyRunning:
		pod.Status.Phase = corev1.PodRunning
	default:
		pod.Status.Phase = corev1.PodPending
	}
}

func setCondition(pod *corev1.Pod, condType corev1.PodConditionType, status corev1.ConditionStatus, now metav1.Time) {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == condType {
			if pod.Status.Conditions[i].Status != status {
				pod.Status.Conditions[i].Status = status
				pod.Status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: now,
	})
}

func restartBackoff(count int) time.Duration {
	d := time.Duration(1<<uint(count-1)) * 10 * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func (c *Controller) ensureLogsResource(ctx context.Context, pod *corev1.Pod, containerName string) {
	logFile := c.runner.LogPath(pod.Namespace, pod.Name, containerName)
	entry := map[string]interface{}{
		"containers": []interface{}{containerName},
		"logsFile":   logFile,
		"follow":     true,
	}

	res := c.dynClient.Resource(logsGVR).Namespace(pod.Namespace)

	existing, err := res.Get(ctx, pod.Name, metav1.GetOptions{})
	if err == nil {
		logs, _, _ := unstructured.NestedSlice(existing.Object, "spec", "logs")
		// Deduplicate: skip if this container already has a log entry.
		for _, logEntry := range logs {
			if entryMap, ok := logEntry.(map[string]interface{}); ok {
				if containers, _ := entryMap["containers"].([]interface{}); len(containers) > 0 && containers[0] == containerName {
					return
				}
			}
		}
		logs = append(logs, entry)
		_ = unstructured.SetNestedSlice(existing.Object, logs, "spec", "logs")
		if _, err := res.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			c.log.Warn("failed to update Logs resource", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
		}
		return
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kwok.x-k8s.io/v1alpha1",
		"kind":       "Logs",
		"metadata": map[string]interface{}{
			"name":      pod.Name,
			"namespace": pod.Namespace,
		},
		"spec": map[string]interface{}{
			"logs": []interface{}{entry},
		},
	}}
	if _, err := res.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		c.log.Warn("failed to create Logs resource", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
	}
}

// noOpStatusSeedable returns true if the pod status has not been populated by
// another controller (e.g. a Kwok Stage). Seedable means the status is still
// in its initial empty/Pending form with no evidence of another writer:
// phase empty or Pending, no startTime, no container/init/ephemeral statuses,
// no networking fields, no reason/message, and at most one condition which must
// be PodScheduled=True.
func noOpStatusSeedable(pod *corev1.Pod) bool {
	if pod.Status.Phase != "" && pod.Status.Phase != corev1.PodPending {
		return false
	}
	if pod.Status.StartTime != nil {
		return false
	}
	if len(pod.Status.ContainerStatuses) > 0 ||
		len(pod.Status.InitContainerStatuses) > 0 ||
		len(pod.Status.EphemeralContainerStatuses) > 0 {
		return false
	}
	if pod.Status.PodIP != "" || pod.Status.HostIP != "" || len(pod.Status.PodIPs) > 0 {
		return false
	}
	if pod.Status.Reason != "" || pod.Status.Message != "" {
		return false
	}
	if len(pod.Status.Conditions) > 1 {
		return false
	}
	if len(pod.Status.Conditions) == 1 {
		c := pod.Status.Conditions[0]
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionTrue {
			return false
		}
	}
	return true
}

func (c *Controller) setPodWaiting(pod *corev1.Pod, reason error) {
	if c.clientset == nil {
		return
	}

	latest, err := c.clientset.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	if err != nil {
		c.log.Warn("failed to get pod for waiting status", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
		return
	}

	now := metav1.NewTime(time.Now())
	latest.Status.Phase = corev1.PodPending
	latest.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, LastTransitionTime: now},
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: now},
	}

	msg := reason.Error()
	var statuses []corev1.ContainerStatus
	for _, cs := range pod.Spec.Containers {
		statuses = append(statuses, corev1.ContainerStatus{
			Name:  cs.Name,
			Image: cs.Image,
			Ready: false,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ContainerCreating",
					Message: msg,
				},
			},
		})
	}
	latest.Status.ContainerStatuses = statuses

	if _, err := c.clientset.CoreV1().Pods(pod.Namespace).UpdateStatus(context.TODO(), latest, metav1.UpdateOptions{}); err != nil {
		c.log.Warn("failed to set pod waiting status", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
	}
}
