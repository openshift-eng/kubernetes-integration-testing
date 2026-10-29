package pod

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"

	corev1 "k8s.io/api/core/v1"
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
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		var statuses []corev1.ContainerStatus
		for _, cs := range pod.Spec.Containers {
			statuses = append(statuses, corev1.ContainerStatus{
				Name:  cs.Name,
				Image: cs.Image,
				Ready: true,
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: now},
				},
			})
		}
		pod.Status.ContainerStatuses = statuses

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

			for i := range pod.Status.ContainerStatuses {
				if pod.Status.ContainerStatuses[i].Name == change.Container {
					pod.Status.ContainerStatuses[i].Ready = false
					pod.Status.ContainerStatuses[i].RestartCount = int32(count)
					pod.Status.ContainerStatuses[i].LastTerminationState = corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   change.ExitCode,
							FinishedAt: now,
						},
					}
					pod.Status.ContainerStatuses[i].State = corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					}
				}
			}

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
			if change.Err != nil {
				pod.Status.Phase = corev1.PodFailed
			} else {
				pod.Status.Phase = corev1.PodSucceeded
			}
			for i := range pod.Status.ContainerStatuses {
				if pod.Status.ContainerStatuses[i].Name == change.Container {
					pod.Status.ContainerStatuses[i].Ready = false
					pod.Status.ContainerStatuses[i].State = corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode:   change.ExitCode,
							FinishedAt: now,
						},
					}
				}
			}
		}
	}

	if _, err := c.clientset.CoreV1().Pods(change.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		c.log.Warn("failed to update pod status", "pod", change.PodName, "namespace", change.Namespace, "state", change.State, "error", err)
	}
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
