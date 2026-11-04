package override

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
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
