package override

import (
	"fmt"
	"reflect"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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
// in place. Outcome is Applied when mutations were satisfied (noOp, replacePod,
// or container overrides), Invalid when a container override matched but could
// not be applied (non-remove targeting a missing container, or removes that
// would leave zero containers), and NoMatch when no override matched.
func Apply(pod *corev1.Pod, overrides []v1alpha1.WorkloadOverride, podNamespace string) Result {
	wo := SelectMatch(overrides, pod, podNamespace)
	if wo == nil {
		return Result{Outcome: NoMatch}
	}

	if wo.Spec.ServiceAccountName != "" {
		pod.Spec.ServiceAccountName = wo.Spec.ServiceAccountName
	}

	if wo.Spec.NoOp {
		ApplyNoOp(pod)
		return Result{Override: wo, Outcome: Applied}
	}

	if wo.Spec.ReplacePod != nil {
		ApplyReplacePod(pod, wo.Spec.ReplacePod)
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
	if IsNoOp(pod) {
		return
	}
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

// buildReplaceContainer constructs the sole desired regular container for a
// PodReplacement. It is the single source of truth for the fields managed by
// ReplacePod: ApplyReplacePod creates new containers from it, and
// matchesReplacePod compares existing containers against it.
//
// A field left at its zero value is unmanaged rather than a request to force
// that zero value. That distinction lets Kubernetes default fields Kit did not
// specify without making an already-applied replacement look like drift.
func buildReplaceContainer(rp *v1alpha1.PodReplacement) corev1.Container {
	replacement := rp.DeepCopy()
	c := corev1.Container{
		Name:    replacement.Name,
		Image:   replacement.Image,
		Command: replacement.Command,
		Args:    replacement.Args,
		Env:     replacement.Env,
	}
	if replacement.PullPolicy != "" {
		c.ImagePullPolicy = replacement.PullPolicy
	}
	return c
}

// ApplyReplacePod replaces all init and regular containers with the single
// container described by rp. Other PodSpec fields, metadata, ownership, and
// scheduling information are preserved.
//
// The operation is idempotent. If the existing container already has every
// field managed by rp, it is left untouched so API-server defaults in
// unmanaged fields survive. Any difference in a managed field replaces the
// workload with the desired replacement.
func ApplyReplacePod(pod *corev1.Pod, rp *v1alpha1.PodReplacement) {
	if matchesReplacePod(pod, rp) {
		return
	}
	pod.Spec.InitContainers = nil
	c := buildReplaceContainer(rp)
	pod.Spec.Containers = []corev1.Container{c}
}

// matchesReplacePod reports whether pod already has the replacement state
// managed by rp. It deliberately does not compare the complete Container
// object: expected zero fields are unmanaged and may have been defaulted by
// the API server.
//
// The comparison works by projecting an existing container onto the fields
// present in expected. Defaults in unmanaged fields are removed only from the
// temporary copy. Missing or different managed fields, mismatched collection
// lengths, missing map keys, and extra collection entries still fail the final
// semantic equality check.
func matchesReplacePod(pod *corev1.Pod, rp *v1alpha1.PodReplacement) bool {
	if len(pod.Spec.InitContainers) != 0 || len(pod.Spec.Containers) != 1 {
		return false
	}

	expected := buildReplaceContainer(rp)
	projected := *pod.Spec.Containers[0].DeepCopy()
	projectManagedFields(reflect.ValueOf(&projected).Elem(), reflect.ValueOf(expected))
	return equality.Semantic.DeepEqual(projected, expected)
}

// projectManagedFields mutates actual in place so it exposes only the fields
// present in expected. It recurses through structs, pointers, slices, arrays,
// and maps; scalar and other leaf values are left for the caller's equality
// check. The function does not make actual equal to expected. Structural
// differences such as different slice lengths or missing or extra map entries
// are intentionally left visible to that check.
//
// A nil or zero expected field means the corresponding actual field is
// unmanaged and is zeroed in the temporary copy. This is what makes
// matchesReplacePod tolerate API-server defaults without encoding knowledge
// about individual Kubernetes defaults.
//
// The function is intentionally private and scoped to ReplacePod. It is not a
// general-purpose merge or patch implementation.
func projectManagedFields(actual, expected reflect.Value) {
	if isAtomicStruct(expected.Type()) {
		return
	}

	switch expected.Kind() {
	case reflect.Struct:
		for i := range expected.NumField() {
			if expected.Field(i).IsZero() {
				actual.Field(i).SetZero()
				continue
			}
			projectManagedFields(actual.Field(i), expected.Field(i))
		}
	case reflect.Pointer:
		if expected.IsNil() {
			actual.SetZero()
		} else if !actual.IsNil() {
			projectManagedFields(actual.Elem(), expected.Elem())
		}
	case reflect.Slice:
		if expected.IsNil() {
			actual.SetZero()
		} else if actual.Len() == expected.Len() {
			for i := range expected.Len() {
				projectManagedFields(actual.Index(i), expected.Index(i))
			}
		}
	case reflect.Array:
		for i := range expected.Len() {
			projectManagedFields(actual.Index(i), expected.Index(i))
		}
	case reflect.Map:
		if expected.IsNil() {
			actual.SetZero()
		} else if !actual.IsNil() {
			for _, key := range expected.MapKeys() {
				actualValue := actual.MapIndex(key)
				if !actualValue.IsValid() {
					continue
				}
				projectedValue := reflect.New(actual.Type().Elem()).Elem()
				projectedValue.Set(actualValue)
				projectManagedFields(projectedValue, expected.MapIndex(key))
				actual.SetMapIndex(key, projectedValue)
			}
		}
	}
}

// isAtomicStruct reports whether t contains state that must not be projected
// field by field with reflection. Kubernetes API values such as
// resource.Quantity have unexported fields; recursing into them would attempt
// to assign values obtained through unexported fields and panic. Such values
// are compared as atomic leaves by the caller's semantic equality check.
func isAtomicStruct(t reflect.Type) bool {
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := range t.NumField() {
		if !t.Field(i).IsExported() {
			return true
		}
	}
	return false
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
