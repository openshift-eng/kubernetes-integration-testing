package pod

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestResolveFieldRef(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "my-ns",
			UID:       types.UID("abc-123"),
		},
		Spec: corev1.PodSpec{
			NodeName:           "node-1",
			ServiceAccountName: "my-sa",
		},
	}

	cases := []struct {
		field string
		want  string
	}{
		{"metadata.name", "my-pod"},
		{"metadata.namespace", "my-ns"},
		{"metadata.uid", "abc-123"},
		{"spec.nodeName", "node-1"},
		{"spec.serviceAccountName", "my-sa"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			val, err := ResolveFieldRef(pod, tc.field)
			require.NoError(t, err)
			assert.Equal(t, tc.want, val)
		})
	}

	t.Run("unsupported field", func(t *testing.T) {
		_, err := ResolveFieldRef(pod, "spec.bogus")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported")
	})
}
