package override

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMatchesPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "mcd-abc",
			Labels: map[string]string{"k8s-app": "machine-config-daemon", "tier": "node"},
		},
		Spec: corev1.PodSpec{
			NodeName: "worker-0",
		},
	}

	tests := []struct {
		name      string
		match     v1alpha1.WorkloadOverrideMatch
		namespace string
		want      bool
	}{
		{
			name:      "namespace match",
			match:     v1alpha1.WorkloadOverrideMatch{Namespace: "openshift-mco"},
			namespace: "openshift-mco",
			want:      true,
		},
		{
			name:      "namespace mismatch",
			match:     v1alpha1.WorkloadOverrideMatch{Namespace: "other"},
			namespace: "openshift-mco",
			want:      false,
		},
		{
			name:      "labels match",
			match:     v1alpha1.WorkloadOverrideMatch{Labels: map[string]string{"k8s-app": "machine-config-daemon"}},
			namespace: "default",
			want:      true,
		},
		{
			name:      "labels partial mismatch",
			match:     v1alpha1.WorkloadOverrideMatch{Labels: map[string]string{"k8s-app": "other"}},
			namespace: "default",
			want:      false,
		},
		{
			name:      "multiple labels all must match",
			match:     v1alpha1.WorkloadOverrideMatch{Labels: map[string]string{"k8s-app": "machine-config-daemon", "tier": "node"}},
			namespace: "default",
			want:      true,
		},
		{
			name:      "pod name match",
			match:     v1alpha1.WorkloadOverrideMatch{PodName: "mcd-abc"},
			namespace: "default",
			want:      true,
		},
		{
			name:      "pod name mismatch",
			match:     v1alpha1.WorkloadOverrideMatch{PodName: "other"},
			namespace: "default",
			want:      false,
		},
		{
			name:      "node name match",
			match:     v1alpha1.WorkloadOverrideMatch{NodeName: "worker-0"},
			namespace: "default",
			want:      true,
		},
		{
			name:      "node name mismatch",
			match:     v1alpha1.WorkloadOverrideMatch{NodeName: "worker-1"},
			namespace: "default",
			want:      false,
		},
		{
			name: "combined match all satisfied",
			match: v1alpha1.WorkloadOverrideMatch{
				Namespace: "openshift-mco",
				Labels:    map[string]string{"k8s-app": "machine-config-daemon"},
				NodeName:  "worker-0",
			},
			namespace: "openshift-mco",
			want:      true,
		},
		{
			name: "combined match one fails",
			match: v1alpha1.WorkloadOverrideMatch{
				Namespace: "openshift-mco",
				Labels:    map[string]string{"k8s-app": "machine-config-daemon"},
				NodeName:  "worker-1",
			},
			namespace: "openshift-mco",
			want:      false,
		},
		{
			name:      "empty namespace matches any",
			match:     v1alpha1.WorkloadOverrideMatch{Labels: map[string]string{"k8s-app": "machine-config-daemon"}},
			namespace: "any-namespace",
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MatchesPod(tt.match, pod, tt.namespace))
		})
	}
}

func TestConstraintCount(t *testing.T) {
	tests := []struct {
		name  string
		match v1alpha1.WorkloadOverrideMatch
		want  int
	}{
		{"empty", v1alpha1.WorkloadOverrideMatch{}, 0},
		{"namespace only", v1alpha1.WorkloadOverrideMatch{Namespace: "ns"}, 1},
		{"one label", v1alpha1.WorkloadOverrideMatch{Labels: map[string]string{"a": "b"}}, 1},
		{"two labels", v1alpha1.WorkloadOverrideMatch{Labels: map[string]string{"a": "b", "c": "d"}}, 2},
		{"all fields", v1alpha1.WorkloadOverrideMatch{
			Namespace: "ns",
			Labels:    map[string]string{"a": "b", "c": "d"},
			PodName:   "pod",
			NodeName:  "node",
		}, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, constraintCount(tt.match))
		})
	}
}

func TestSelectMatch(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "mcd-abc",
			Labels: map[string]string{"k8s-app": "machine-config-daemon"},
		},
		Spec: corev1.PodSpec{NodeName: "worker-0"},
	}

	t.Run("no match returns nil", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "wrong"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{Namespace: "kube-system"},
					NoOp:  true,
				},
			},
		}
		assert.Nil(t, SelectMatch(overrides, pod, "default"))
	})

	t.Run("most specific wins", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "broad"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{Namespace: "openshift-mco"},
					NoOp:  true,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "specific"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{
						Namespace: "openshift-mco",
						Labels:    map[string]string{"k8s-app": "machine-config-daemon"},
					},
					NoOp: true,
				},
			},
		}
		got := SelectMatch(overrides, pod, "openshift-mco")
		require.NotNil(t, got)
		assert.Equal(t, "specific", got.Name)
	})

	t.Run("tie broken by name", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "zebra"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{Namespace: "openshift-mco"},
					NoOp:  true,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{Namespace: "openshift-mco"},
					NoOp:  true,
				},
			},
		}
		got := SelectMatch(overrides, pod, "openshift-mco")
		require.NotNil(t, got)
		assert.Equal(t, "alpha", got.Name)
	})

	t.Run("single match", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "only"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{
						Labels: map[string]string{"k8s-app": "machine-config-daemon"},
					},
					NoOp: true,
				},
			},
		}
		got := SelectMatch(overrides, pod, "default")
		require.NotNil(t, got)
		assert.Equal(t, "only", got.Name)
	})

	t.Run("noOp has no special precedence", func(t *testing.T) {
		overrides := []v1alpha1.WorkloadOverride{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "broad-noop"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{Namespace: "openshift-mco"},
					NoOp:  true,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "specific-override"},
				Spec: v1alpha1.WorkloadOverrideSpec{
					Match: v1alpha1.WorkloadOverrideMatch{
						Namespace: "openshift-mco",
						Labels:    map[string]string{"k8s-app": "machine-config-daemon"},
					},
					Containers: []v1alpha1.ContainerOverride{
						{Name: "mcd", Image: "stub:latest"},
					},
				},
			},
		}
		got := SelectMatch(overrides, pod, "openshift-mco")
		require.NotNil(t, got)
		assert.Equal(t, "specific-override", got.Name)
	})
}
