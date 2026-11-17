package override

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplyNoOp(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init", Image: "init:latest"},
			},
			Containers: []corev1.Container{
				{
					Name:           "main",
					Image:          "main:latest",
					LivenessProbe:  &corev1.Probe{},
					ReadinessProbe: &corev1.Probe{},
				},
				{
					Name:  "sidecar",
					Image: "sidecar:latest",
				},
			},
		},
	}

	ApplyNoOp(pod)

	assert.Empty(t, pod.Spec.InitContainers)
	require.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, "kit-noop", pod.Spec.Containers[0].Name)
	assert.Equal(t, NoOpImage, pod.Spec.Containers[0].Image)
}

func TestApplyContainerOverrides(t *testing.T) {
	t.Run("image replacement", func(t *testing.T) {
		pod := podWithContainers("mcd", "quay.io/mcd:real")
		outcome, _ := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "mcd", Image: "quay.io/mcd:stub"},
		})
		assert.Equal(t, Applied, outcome)
		assert.Equal(t, "quay.io/mcd:stub", pod.Spec.Containers[0].Image)
	})

	t.Run("args replacement", func(t *testing.T) {
		pod := podWithContainers("mcd", "img")
		pod.Spec.Containers[0].Args = []string{"--real"}
		outcome, _ := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "mcd", Args: []string{"--stub-mode", "--skip-reboot"}},
		})
		assert.Equal(t, Applied, outcome)
		assert.Equal(t, []string{"--stub-mode", "--skip-reboot"}, pod.Spec.Containers[0].Args)
	})

	t.Run("command replacement", func(t *testing.T) {
		pod := podWithContainers("mcd", "img")
		pod.Spec.Containers[0].Command = []string{"/usr/bin/mcd"}
		outcome, _ := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "mcd", Command: []string{"/usr/bin/mcd-stub"}},
		})
		assert.Equal(t, Applied, outcome)
		assert.Equal(t, []string{"/usr/bin/mcd-stub"}, pod.Spec.Containers[0].Command)
	})

	t.Run("pull policy", func(t *testing.T) {
		pod := podWithContainers("mcd", "img")
		outcome, _ := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "mcd", PullPolicy: corev1.PullNever},
		})
		assert.Equal(t, Applied, outcome)
		assert.Equal(t, corev1.PullNever, pod.Spec.Containers[0].ImagePullPolicy)
	})

	t.Run("remove container", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "mcd", Image: "mcd:latest"},
					{Name: "proxy", Image: "proxy:latest"},
				},
			},
		}
		outcome, _ := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "mcd", Remove: true},
		})
		assert.Equal(t, Applied, outcome)
		require.Len(t, pod.Spec.Containers, 1)
		assert.Equal(t, "proxy", pod.Spec.Containers[0].Name)
	})

	t.Run("no match leaves pod unchanged and returns invalid", func(t *testing.T) {
		pod := podWithContainers("other", "img")
		outcome, reason := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "mcd", Image: "quay.io/mcd:stub"},
		})
		assert.Equal(t, Invalid, outcome)
		assert.Contains(t, reason, "mcd")
		assert.Equal(t, "img", pod.Spec.Containers[0].Image)
	})
}

func TestMergeEnv(t *testing.T) {
	existing := []corev1.EnvVar{
		{Name: "A", Value: "1"},
		{Name: "B", Value: "2"},
		{Name: "C", Value: "3"},
	}

	t.Run("replace existing", func(t *testing.T) {
		result := MergeEnv(existing, []v1alpha1.EnvOverride{
			{Name: "B", Value: "replaced"},
		})
		assertEnv(t, result, "A", "1")
		assertEnv(t, result, "B", "replaced")
		assertEnv(t, result, "C", "3")
	})

	t.Run("remove existing", func(t *testing.T) {
		result := MergeEnv(existing, []v1alpha1.EnvOverride{
			{Name: "B", Remove: true},
		})
		require.Len(t, result, 2)
		assertEnv(t, result, "A", "1")
		assertEnv(t, result, "C", "3")
	})

	t.Run("add new", func(t *testing.T) {
		result := MergeEnv(existing, []v1alpha1.EnvOverride{
			{Name: "D", Value: "new"},
		})
		require.Len(t, result, 4)
		assertEnv(t, result, "D", "new")
	})

	t.Run("combined operations", func(t *testing.T) {
		result := MergeEnv(existing, []v1alpha1.EnvOverride{
			{Name: "A", Value: "updated"},
			{Name: "B", Remove: true},
			{Name: "D", Value: "added"},
		})
		require.Len(t, result, 3)
		assertEnv(t, result, "A", "updated")
		assertEnv(t, result, "C", "3")
		assertEnv(t, result, "D", "added")
	})

	t.Run("remove nonexistent is no-op", func(t *testing.T) {
		result := MergeEnv(existing, []v1alpha1.EnvOverride{
			{Name: "Z", Remove: true},
		})
		assert.Len(t, result, 3)
	})
}

func TestApply(t *testing.T) {
	overrides := []v1alpha1.WorkloadOverride{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mcd-noop"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "openshift-mco"},
				NoOp:  true,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "swap-image"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Labels: map[string]string{"app": "proxy"}},
				Containers: []v1alpha1.ContainerOverride{
					{Name: "proxy", Image: "proxy:stub"},
				},
			},
		},
	}

	t.Run("no match", func(t *testing.T) {
		pod := podWithContainers("unrelated", "img")
		r := Apply(pod, overrides, "default")
		assert.Equal(t, NoMatch, r.Outcome)
		assert.Nil(t, r.Override)
	})

	t.Run("noOp suppresses pod", func(t *testing.T) {
		pod := podWithContainers("mcd", "mcd:real")
		r := Apply(pod, overrides, "openshift-mco")
		require.Equal(t, Applied, r.Outcome)
		assert.Equal(t, "mcd-noop", r.Override.Name)
		require.Len(t, pod.Spec.Containers, 1)
		assert.Equal(t, "kit-noop", pod.Spec.Containers[0].Name)
	})

	t.Run("container override mutates pod", func(t *testing.T) {
		pod := podWithContainers("proxy", "proxy:real")
		pod.Labels = map[string]string{"app": "proxy"}
		r := Apply(pod, overrides, "default")
		require.Equal(t, Applied, r.Outcome)
		assert.Equal(t, "swap-image", r.Override.Name)
		assert.Equal(t, "proxy:stub", pod.Spec.Containers[0].Image)
	})

	t.Run("non-remove targeting missing container is invalid", func(t *testing.T) {
		pod := podWithContainers("other", "img")
		pod.Labels = map[string]string{"app": "proxy"}
		r := Apply(pod, overrides, "default")
		assert.Equal(t, Invalid, r.Outcome)
		require.NotNil(t, r.Override)
	})

	t.Run("remove of missing container is satisfied", func(t *testing.T) {
		removeOverrides := []v1alpha1.WorkloadOverride{{
			ObjectMeta: metav1.ObjectMeta{Name: "remove-gone"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match:      v1alpha1.WorkloadOverrideMatch{Namespace: "ns"},
				Containers: []v1alpha1.ContainerOverride{{Name: "nonexistent", Remove: true}},
			},
		}}
		pod := podWithContainers("app", "img")
		r := Apply(pod, removeOverrides, "ns")
		assert.Equal(t, Applied, r.Outcome)
		assert.Len(t, pod.Spec.Containers, 1)
	})

	t.Run("remove leaving zero containers is invalid", func(t *testing.T) {
		removeOverrides := []v1alpha1.WorkloadOverride{{
			ObjectMeta: metav1.ObjectMeta{Name: "remove-all"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match:      v1alpha1.WorkloadOverrideMatch{Namespace: "ns"},
				Containers: []v1alpha1.ContainerOverride{{Name: "only", Remove: true}},
			},
		}}
		pod := podWithContainers("only", "img")
		r := Apply(pod, removeOverrides, "ns")
		assert.Equal(t, Invalid, r.Outcome)
		assert.Len(t, pod.Spec.Containers, 1)
	})

	t.Run("tie selects most specific", func(t *testing.T) {
		both := []v1alpha1.WorkloadOverride{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "broad"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match:      v1alpha1.WorkloadOverrideMatch{Namespace: "ns"},
					Containers: []v1alpha1.ContainerOverride{{Name: "c", Image: "broad:v1"}},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "specific"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{
						Namespace: "ns",
						Labels:    map[string]string{"app": "x"},
					},
					Containers: []v1alpha1.ContainerOverride{{Name: "c", Image: "specific:v1"}},
				},
			},
		}
		pod := podWithContainers("c", "original")
		pod.Labels = map[string]string{"app": "x"}
		r := Apply(pod, both, "ns")
		require.NotNil(t, r.Override)
		assert.Equal(t, "specific", r.Override.Name)
		assert.Equal(t, "specific:v1", pod.Spec.Containers[0].Image)
	})
}

func TestApplyContainerOverridesValidation(t *testing.T) {
	t.Run("remove last container is invalid", func(t *testing.T) {
		pod := podWithContainers("only", "img")
		outcome, reason := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "only", Remove: true},
		})
		assert.Equal(t, Invalid, outcome)
		assert.Contains(t, reason, "empty")
		assert.Len(t, pod.Spec.Containers, 1)
	})

	t.Run("non-remove targeting missing container is invalid", func(t *testing.T) {
		pod := podWithContainers("app", "img")
		outcome, reason := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "nonexistent", Image: "new:v1"},
		})
		assert.Equal(t, Invalid, outcome)
		assert.Contains(t, reason, "nonexistent")
	})

	t.Run("remove of missing container is satisfied", func(t *testing.T) {
		pod := podWithContainers("app", "img")
		outcome, _ := ApplyContainerOverrides(pod, []v1alpha1.ContainerOverride{
			{Name: "gone", Remove: true},
		})
		assert.Equal(t, Applied, outcome)
	})
}

func TestApplyReplacePod(t *testing.T) {
	rp := &v1alpha1.PodReplacement{
		Name:    "agent",
		Image:   "quay.io/kit-agent:latest",
		Command: []string{"/kit-agent"},
		Args:    []string{"--bundle=/tmp/bundle"},
		Env: []corev1.EnvVar{
			{Name: "KIT_AGENT_BUNDLE_URL", Value: "http://controller:8082/agents/test/bundle"},
		},
		PullPolicy: corev1.PullAlways,
	}

	t.Run("replaces all containers with single agent container", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "init", Image: "init:latest"},
				},
				Containers: []corev1.Container{
					{Name: "main", Image: "main:latest"},
					{Name: "sidecar", Image: "sidecar:latest"},
				},
			},
		}

		ApplyReplacePod(pod, rp)

		assert.Empty(t, pod.Spec.InitContainers)
		require.Len(t, pod.Spec.Containers, 1)
		c := pod.Spec.Containers[0]
		assert.Equal(t, "agent", c.Name)
		assert.Equal(t, "quay.io/kit-agent:latest", c.Image)
		assert.Equal(t, []string{"/kit-agent"}, c.Command)
		assert.Equal(t, []string{"--bundle=/tmp/bundle"}, c.Args)
		assert.Equal(t, corev1.PullAlways, c.ImagePullPolicy)
		require.Len(t, c.Env, 1)
		assert.Equal(t, "KIT_AGENT_BUNDLE_URL", c.Env[0].Name)
	})

	t.Run("omitted pull policy keeps container default", func(t *testing.T) {
		pod := podWithContainers("main", "main:latest")
		rp := &v1alpha1.PodReplacement{Name: "agent", Image: "img:v1"}
		ApplyReplacePod(pod, rp)
		assert.Equal(t, corev1.PullPolicy(""), pod.Spec.Containers[0].ImagePullPolicy)
	})

	t.Run("does not touch service account", func(t *testing.T) {
		pod := podWithContainers("main", "main:latest")
		pod.Spec.ServiceAccountName = "original"
		rp := &v1alpha1.PodReplacement{Name: "agent", Image: "img:v1"}
		ApplyReplacePod(pod, rp)
		assert.Equal(t, "original", pod.Spec.ServiceAccountName)
	})
}

// TestApplyReplacePodIsIdempotentAfterDefaulting verifies the ownership
// contract of ApplyReplacePod: differences limited to unmanaged fields do not
// rebuild an already-applied replacement, while any change to a managed field
// or to the shape of a managed collection does.
func TestApplyReplacePodIsIdempotentAfterDefaulting(t *testing.T) {
	rp := &v1alpha1.PodReplacement{
		Name:    "agent",
		Image:   "quay.io/kit-agent:latest",
		Command: []string{"/usr/bin/kit-agent"},
		Args:    []string{"--bundle=/tmp/bundle"},
		Env: []corev1.EnvVar{{
			Name:  "KIT_AGENT_BUNDLE_URL",
			Value: "http://controller:8082/bundle",
		}, {
			Name: "KIT_POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		}, {
			Name: "KIT_CPU_LIMIT",
			ValueFrom: &corev1.EnvVarSource{
				ResourceFieldRef: &corev1.ResourceFieldSelector{
					Resource: "limits.cpu",
					Divisor:  resource.MustParse("1"),
				},
			},
		}},
		PullPolicy: corev1.PullAlways,
	}

	// 1. Apply once and verify that the managed fields change the Pod.
	pod := podWithContainers("main", "main:latest")
	originalPod := pod.DeepCopy()
	ApplyReplacePod(pod, rp)

	// The PodReplacement has been applied, so the workload must have changed.
	assert.False(t, equality.Semantic.DeepEqual(originalPod.Spec, pod.Spec))

	// 2. Add representative values to fields that the replacement does not
	// manage. This simulates Kubernetes injecting defaults after a Pod is stored.
	container := &pod.Spec.Containers[0]
	container.TerminationMessagePath = "/dev/termination-log"
	container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	envByName(t, container.Env, "KIT_POD_NAME").ValueFrom.FieldRef.APIVersion = "v1"
	envByName(t, container.Env, "KIT_CPU_LIMIT").ValueFrom.ResourceFieldRef.ContainerName = "agent"
	before := pod.DeepCopy()

	// Apply again: the Pod must remain unchanged because only unmanaged fields
	// were modified.
	ApplyReplacePod(pod, rp)
	assert.True(t, equality.Semantic.DeepEqual(before.Spec, pod.Spec))

	// 3. An extra entry changes the shape of a managed collection and must be
	// removed when the replacement is reapplied.
	pod.Spec.Containers[0].Env = append(
		pod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "KIT_UNEXPECTED", Value: "unexpected"},
	)
	ApplyReplacePod(pod, rp)
	assert.Len(t, pod.Spec.Containers[0].Env, len(rp.Env))
	for _, env := range pod.Spec.Containers[0].Env {
		assert.NotEqual(t, "KIT_UNEXPECTED", env.Name)
	}

	// 4. Changes to managed fields must also be repaired.
	pod.Spec.Containers[0].Command = []string{"/hacked"}
	envByName(t, pod.Spec.Containers[0].Env, "KIT_AGENT_BUNDLE_URL").Value = "http://hacked/bundle"
	ApplyReplacePod(pod, rp)
	assert.Equal(t, rp.Command, pod.Spec.Containers[0].Command)
	assertEnv(t, pod.Spec.Containers[0].Env, "KIT_AGENT_BUNDLE_URL", "http://controller:8082/bundle")
}

func TestApplyReplacePodIntegration(t *testing.T) {
	overrides := []v1alpha1.WorkloadOverride{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "replace-mcd"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "openshift-mco"},
				ReplacePod: &v1alpha1.PodReplacement{
					Name:  "agent",
					Image: "quay.io/kit-agent:latest",
				},
			},
		},
	}

	t.Run("Apply dispatches to replacePod", func(t *testing.T) {
		pod := podWithContainers("mcd", "mcd:real")
		r := Apply(pod, overrides, "openshift-mco")
		require.Equal(t, Applied, r.Outcome)
		assert.Equal(t, "replace-mcd", r.Override.Name)
		require.Len(t, pod.Spec.Containers, 1)
		assert.Equal(t, "agent", pod.Spec.Containers[0].Name)
	})
}

func TestApplyServiceAccountName(t *testing.T) {
	t.Run("sets SA with containers mode", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{{
			ObjectMeta: metav1.ObjectMeta{Name: "sa-override"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match:              v1alpha1.WorkloadOverrideMatch{Namespace: "ns"},
				Containers:         []v1alpha1.ContainerOverride{{Name: "app", Image: "new:v1"}},
				ServiceAccountName: "kit-stub",
			},
		}}
		pod := podWithContainers("app", "old:v1")
		r := Apply(pod, overrides, "ns")
		require.Equal(t, Applied, r.Outcome)
		assert.Equal(t, "kit-stub", pod.Spec.ServiceAccountName)
		assert.Equal(t, "new:v1", pod.Spec.Containers[0].Image)
	})

	t.Run("sets SA with replacePod mode", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{{
			ObjectMeta: metav1.ObjectMeta{Name: "sa-replace"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match:              v1alpha1.WorkloadOverrideMatch{Namespace: "ns"},
				ReplacePod:         &v1alpha1.PodReplacement{Name: "agent", Image: "agent:v1"},
				ServiceAccountName: "kit-agent",
			},
		}}
		pod := podWithContainers("app", "old:v1")
		r := Apply(pod, overrides, "ns")
		require.Equal(t, Applied, r.Outcome)
		assert.Equal(t, "kit-agent", pod.Spec.ServiceAccountName)
	})

	t.Run("sets SA with noOp mode", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{{
			ObjectMeta: metav1.ObjectMeta{Name: "sa-noop"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match:              v1alpha1.WorkloadOverrideMatch{Namespace: "ns"},
				NoOp:               true,
				ServiceAccountName: "kit-noop-sa",
			},
		}}
		pod := podWithContainers("app", "old:v1")
		r := Apply(pod, overrides, "ns")
		require.Equal(t, Applied, r.Outcome)
		assert.Equal(t, "kit-noop-sa", pod.Spec.ServiceAccountName)
	})

	t.Run("omitted SA preserves original", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{{
			ObjectMeta: metav1.ObjectMeta{Name: "no-sa"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match:      v1alpha1.WorkloadOverrideMatch{Namespace: "ns"},
				Containers: []v1alpha1.ContainerOverride{{Name: "app", Image: "new:v1"}},
			},
		}}
		pod := podWithContainers("app", "old:v1")
		pod.Spec.ServiceAccountName = "original"
		r := Apply(pod, overrides, "ns")
		require.Equal(t, Applied, r.Outcome)
		assert.Equal(t, "original", pod.Spec.ServiceAccountName)
	})
}

func TestIsNoOp(t *testing.T) {
	t.Run("canonical noOp pod", func(t *testing.T) {
		pod := podWithContainers("original", "original:latest")
		ApplyNoOp(pod)
		assert.True(t, IsNoOp(pod))
	})

	t.Run("regular pod", func(t *testing.T) {
		pod := podWithContainers("app", "app:latest")
		assert.False(t, IsNoOp(pod))
	})

	t.Run("pod with init containers is not noOp", func(t *testing.T) {
		pod := podWithContainers("original", "original:latest")
		ApplyNoOp(pod)
		pod.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "init:latest"}}
		assert.False(t, IsNoOp(pod))
	})

	t.Run("noOp container with probe is not canonical", func(t *testing.T) {
		pod := podWithContainers("original", "original:latest")
		ApplyNoOp(pod)
		pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{}
		assert.False(t, IsNoOp(pod))
	})
}

func podWithContainers(name, image string) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: name, Image: image},
			},
		},
	}
}

func assertEnv(t *testing.T, envs []corev1.EnvVar, name, value string) {
	t.Helper()
	for _, e := range envs {
		if e.Name == name {
			assert.Equal(t, value, e.Value, "env %s", name)
			return
		}
	}
	t.Errorf("env %s not found", name)
}

func envByName(t *testing.T, envs []corev1.EnvVar, name string) *corev1.EnvVar {
	t.Helper()
	for i := range envs {
		if envs[i].Name == name {
			return &envs[i]
		}
	}
	t.Fatalf("env %s not found", name)
	return nil
}
