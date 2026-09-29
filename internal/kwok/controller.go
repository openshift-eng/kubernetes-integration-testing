package kwok

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var logsGVR = schema.GroupVersionResource{
	Group:    "kwok.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "logs",
}

type controller struct {
	kubeconfig string
	runner     PodRunner
	changes    <-chan ContainerStateChange
	log        *slog.Logger
	clientset  kubernetes.Interface
	ctx        context.Context
	dynClient  dynamic.Interface
	restartsMu sync.Mutex
	restarts   map[string]int
}

func newController(kubeconfig string, runner PodRunner, changes <-chan ContainerStateChange, log *slog.Logger) *controller {
	return &controller{
		kubeconfig: kubeconfig,
		runner:     runner,
		changes:    changes,
		log:        log,
		restarts:   make(map[string]int),
	}
}

func (c *controller) run(ctx context.Context) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", c.kubeconfig)
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating clientset: %w", err)
	}
	c.clientset = clientset
	c.ctx = ctx

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}
	c.dynClient = dynClient

	go c.processStateChanges()

	factory := informers.NewSharedInformerFactory(clientset, 0)
	podInformer := factory.Core().V1().Pods().Informer()

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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
				c.runner.StopPod(string(pod.UID))
			}
		},
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	<-ctx.Done()
	c.runner.StopAll()
	return nil
}

func (c *controller) handlePod(ctx context.Context, pod *corev1.Pod) {
	if pod.Spec.NodeName == "" {
		return
	}
	if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return
	}

	pod = pod.DeepCopy()
	go func() {
		if err := c.runner.RunPod(ctx, pod); err != nil {
			c.log.Error("failed to run pod", "pod", pod.Name, "namespace", pod.Namespace, "error", err)
		}
	}()
}

func (c *controller) processStateChanges() {
	for change := range c.changes {
		c.onStateChange(change)
	}
}

func (c *controller) onStateChange(change ContainerStateChange) {
	if c.clientset == nil {
		return
	}

	now := metav1.NewTime(time.Now())

	pod, err := c.clientset.CoreV1().Pods(change.Namespace).Get(context.TODO(), change.PodName, metav1.GetOptions{})
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

		c.ensureLogsResource(c.ctx, pod, change.Container)

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
				case <-c.ctx.Done():
					return
				}
				pod, err := c.clientset.CoreV1().Pods(change.Namespace).Get(c.ctx, change.PodName, metav1.GetOptions{})
				if err != nil {
					return
				}
				if err := c.runner.RunPod(c.ctx, pod); err != nil {
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

	if _, err := c.clientset.CoreV1().Pods(change.Namespace).UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{}); err != nil {
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

func (c *controller) ensureLogsResource(ctx context.Context, pod *corev1.Pod, containerName string) {
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
		unstructured.SetNestedSlice(existing.Object, logs, "spec", "logs")
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
