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
)

var logsGVR = schema.GroupVersionResource{
	Group:    "kwok.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "logs",
}

type Controller struct {
	runner     PodRunner
	log        *slog.Logger
	clientset  kubernetes.Interface
	dynClient  dynamic.Interface
	restartsMu sync.Mutex
	restarts   map[string]int
	// claimed tracks pod UIDs for which RunPod is already in progress,
	// preventing duplicate goroutines from concurrent informer events.
	claimedMu sync.Mutex
	claimed   map[string]bool
}

func NewController(clients *kube.Clients, runner PodRunner, log *slog.Logger) *Controller {
	return &Controller{
		runner:    runner,
		log:       log,
		clientset: clients.Clientset,
		dynClient: clients.Dynamic,
		restarts:  make(map[string]int),
		claimed:   make(map[string]bool),
	}
}

func (c *Controller) Run(ctx context.Context) error {
	go c.processStateChanges(ctx, c.runner.Changes())

	factory := informers.NewSharedInformerFactory(c.clientset, 0)
	podInformer := factory.Core().V1().Pods().Informer()

	_, _ = podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if pod, ok := obj.(*corev1.Pod); ok {
				c.handlePod(ctx, pod)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if pod, ok := newObj.(*corev1.Pod); ok {
				c.handlePod(ctx, pod)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					pod, _ = tombstone.Obj.(*corev1.Pod)
				}
			}
			if pod != nil {
				uid := string(pod.UID)
				c.claimedMu.Lock()
				delete(c.claimed, uid)
				c.claimedMu.Unlock()
				c.runner.StopPod(uid)
			}
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	<-ctx.Done()
	return nil
}

func (c *Controller) handlePod(ctx context.Context, pod *corev1.Pod) {
	if pod.Spec.NodeName == "" {
		return
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return
	}

	uid := string(pod.UID)
	c.claimedMu.Lock()
	if c.claimed[uid] {
		c.claimedMu.Unlock()
		return
	}
	c.claimed[uid] = true
	c.claimedMu.Unlock()

	pod = pod.DeepCopy()

	if override.IsNoOp(pod) {
		go c.setNoOpStatus(ctx, pod)
		return
	}

	go func() {
		if err := c.runner.RunPod(ctx, pod); err != nil {
			c.log.Error("failed to run pod", "pod", pod.Name, "namespace", pod.Namespace, "error", err)

			c.claimedMu.Lock()
			delete(c.claimed, uid)
			c.claimedMu.Unlock()

			c.setPodWaiting(pod, err)
		}
	}()
}

func (c *Controller) unclaimPod(uid string) {
	c.claimedMu.Lock()
	delete(c.claimed, uid)
	c.claimedMu.Unlock()
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

			go func() {
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				pod, err := c.clientset.CoreV1().Pods(change.Namespace).Get(ctx, change.PodName, metav1.GetOptions{})
				if err != nil {
					return
				}
				if err := c.runner.RunPod(ctx, pod); err != nil {
					c.log.Error("failed to restart pod", "pod", pod.Name, "namespace", pod.Namespace, "attempt", count, "error", err)
				}
			}()
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

const noOpStatusMaxRetries = 3

func (c *Controller) setNoOpStatus(ctx context.Context, pod *corev1.Pod) {
	uid := pod.UID

	for attempt := 0; attempt < noOpStatusMaxRetries; attempt++ {
		latest, err := c.clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			if isTransientError(err) && attempt < noOpStatusMaxRetries-1 {
				c.log.Info("transient error getting pod for no-op status, retrying", "pod", pod.Name, "namespace", pod.Namespace, "error", err, "attempt", attempt+1)
				continue
			}
			c.log.Warn("failed to get pod for no-op status", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
			c.unclaimPod(string(uid))
			return
		}

		if latest.UID != uid {
			c.unclaimPod(string(uid))
			return
		}

		if !override.IsNoOp(latest) {
			c.unclaimPod(string(uid))
			return
		}

		if !noOpStatusSeedable(latest) {
			c.log.Info("no-op pod status already populated, yielding", "pod", pod.Name, "namespace", pod.Namespace)
			return
		}

		now := metav1.NewTime(time.Now())
		latest.Status.Phase = corev1.PodRunning
		latest.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		latest.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  override.NoOpContainerName,
			Image: override.NoOpImage,
			Ready: true,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: now},
			},
		}}

		_, err = c.clientset.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		if err == nil {
			c.log.Info("set no-op pod status", "pod", pod.Name, "namespace", pod.Namespace)
			return
		}

		if apierrors.IsConflict(err) || isTransientError(err) {
			c.log.Info("no-op status write failed, retrying", "pod", pod.Name, "namespace", pod.Namespace, "error", err, "attempt", attempt+1)
			continue
		}

		c.log.Warn("failed to set no-op pod status", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
		c.unclaimPod(string(uid))
		return
	}

	c.log.Warn("no-op status retries exhausted", "pod", pod.Name, "namespace", pod.Namespace)
	c.unclaimPod(string(uid))
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

func isTransientError(err error) bool {
	return apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err)
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
