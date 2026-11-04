package override

import (
	"fmt"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
)

const NoOpImage = "registry.k8s.io/pause:3.10"

type Outcome int

const (
	NoMatch Outcome = iota
	Applied
	Invalid
)

type Result struct {
	Override *v1alpha1.WorkloadOverride
	Outcome  Outcome
	Reason   string
}

// Apply selects the most specific matching WorkloadOverride and mutates the pod
// in place. Outcome is Applied when mutations were satisfied, Invalid when the
// override matched but could not be applied (non-remove targeting a missing
// container, or removes that would leave zero containers), and NoMatch when no
// override matched.
func Apply(pod *corev1.Pod, overrides []v1alpha1.WorkloadOverride, podNamespace string) Result {
	wo := SelectMatch(overrides, pod, podNamespace)
	if wo == nil {
		return Result{Outcome: NoMatch}
	}

	if wo.Spec.NoOp {
		ApplyNoOp(pod)
		return Result{Override: wo, Outcome: Applied}
	}

	outcome, reason := ApplyContainerOverrides(pod, wo.Spec.Containers)
	return Result{Override: wo, Outcome: outcome, Reason: reason}
}

const NoOpContainerName = "kit-noop"

// ApplyNoOp replaces the pod's workload with a single idle container.
// The pod keeps its metadata, labels, owner references, scheduling fields,
// and restart policy — only the container list changes.
func ApplyNoOp(pod *corev1.Pod) {
	pod.Spec.InitContainers = nil
	pod.Spec.Containers = []corev1.Container{{
		Name:    NoOpContainerName,
		Image:   NoOpImage,
		Command: []string{"/pause"},
	}}
}

// IsNoOp reports whether the pod has the canonical no-op shape:
// no init containers, exactly one container named kit-noop running the
// kit-managed pause image with no probes.
func IsNoOp(pod *corev1.Pod) bool {
	if len(pod.Spec.InitContainers) != 0 {
		return false
	}
	if len(pod.Spec.Containers) != 1 {
		return false
	}
	c := &pod.Spec.Containers[0]
	return c.Name == NoOpContainerName &&
		c.Image == NoOpImage &&
		len(c.Command) == 1 && c.Command[0] == "/pause" &&
		len(c.Args) == 0 &&
		c.LivenessProbe == nil &&
		c.ReadinessProbe == nil &&
		c.StartupProbe == nil
}

func ApplyContainerOverrides(pod *corev1.Pod, overrides []v1alpha1.ContainerOverride) (Outcome, string) {
	existing := make(map[string]bool, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		existing[c.Name] = true
	}

	for _, co := range overrides {
		if !co.Remove && !existing[co.Name] {
			return Invalid, fmt.Sprintf("container %q not found in pod", co.Name)
		}
	}

	remaining := len(pod.Spec.Containers)
	for _, co := range overrides {
		if co.Remove && existing[co.Name] {
			remaining--
		}
	}
	if remaining <= 0 {
		return Invalid, "removing all containers would leave pod empty"
	}

	for _, co := range overrides {
		if co.Remove {
			RemoveContainer(pod, co.Name)
			continue
		}
		for i := range pod.Spec.Containers {
			c := &pod.Spec.Containers[i]
			if c.Name != co.Name {
				continue
			}
			if co.Image != "" {
				c.Image = co.Image
			}
			if co.PullPolicy != "" {
				c.ImagePullPolicy = co.PullPolicy
			}
			if co.Command != nil {
				c.Command = co.Command
			}
			if co.Args != nil {
				c.Args = co.Args
			}
			if len(co.Env) > 0 {
				c.Env = MergeEnv(c.Env, co.Env)
			}
		}
	}
	return Applied, ""
}

func RemoveContainer(pod *corev1.Pod, name string) {
	containers := make([]corev1.Container, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		if c.Name != name {
			containers = append(containers, c)
		}
	}
	pod.Spec.Containers = containers
}

func MergeEnv(existing []corev1.EnvVar, overrides []v1alpha1.EnvOverride) []corev1.EnvVar {
	result := make([]corev1.EnvVar, 0, len(existing))
	removed := make(map[string]bool)
	replaced := make(map[string]string)

	for _, o := range overrides {
		if o.Remove {
			removed[o.Name] = true
		} else {
			replaced[o.Name] = o.Value
		}
	}

	seen := make(map[string]bool)
	for _, e := range existing {
		if removed[e.Name] {
			continue
		}
		if val, ok := replaced[e.Name]; ok {
			result = append(result, corev1.EnvVar{Name: e.Name, Value: val})
			seen[e.Name] = true
			continue
		}
		result = append(result, e)
		seen[e.Name] = true
	}

	for _, o := range overrides {
		if !o.Remove && !seen[o.Name] {
			result = append(result, corev1.EnvVar{Name: o.Name, Value: o.Value})
		}
	}

	return result
}
