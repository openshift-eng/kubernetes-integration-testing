package kwok

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

type ContainerState int

const (
	ContainerStarted ContainerState = iota
	ContainerExited
)

type ContainerStateChange struct {
	PodUID    string
	PodName   string
	Namespace string
	Container string
	State     ContainerState
	ExitCode  int32
	Err       error
}

type PodRunner interface {
	RunPod(ctx context.Context, pod *corev1.Pod) error
	StopPod(uid string)
	StopAll()
	LogPath(namespace, pod, container string) string
}
