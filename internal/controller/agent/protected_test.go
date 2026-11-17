package agent_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	agentctl "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testDigest = "aabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccdd"
const testDigest2 = "1122334411223344112233441122334411223344112233441122334411223344"

func TestProtectedBundleState(t *testing.T) {
	t.Run("extracts digest from replacePod env", func(t *testing.T) {
		wo := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{Name: "wo-1"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "default"},
				ReplacePod: &v1alpha1.PodReplacement{
					Name:  "kit-agent",
					Image: "img",
					Env: []corev1.EnvVar{
						{Name: "KIT_AGENT_BUNDLE_SHA256", Value: testDigest},
						{Name: "OTHER_VAR", Value: "ignored"},
					},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wo).Build()
		snap, err := agentctl.ProtectedBundleState(c)(context.Background())
		require.NoError(t, err)

		_, ok := snap.ProtectedDigests[testDigest]
		assert.True(t, ok, "digest from replacePod env must be protected")
		assert.Len(t, snap.ProtectedDigests, 1)
	})

	t.Run("extracts digest from container override env", func(t *testing.T) {
		wo := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{Name: "wo-1"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "default"},
				Containers: []v1alpha1.ContainerOverride{{
					Name:  "agent",
					Image: "img",
					Env: []v1alpha1.EnvOverride{
						{Name: "KIT_AGENT_BUNDLE_SHA256", Value: testDigest},
					},
				}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wo).Build()
		snap, err := agentctl.ProtectedBundleState(c)(context.Background())
		require.NoError(t, err)

		_, ok := snap.ProtectedDigests[testDigest]
		assert.True(t, ok, "digest from container override env must be protected")
	})

	t.Run("extracts digest and name from agent", func(t *testing.T) {
		ag := &v1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
			Spec: v1alpha1.AgentSpec{
				Script: v1alpha1.AgentScriptRef{Collection: "c", Package: "p"},
				Target: v1alpha1.AgentTarget{Namespace: "default"},
			},
			Status: v1alpha1.AgentStatus{
				BundleDigest: testDigest,
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ag).Build()
		snap, err := agentctl.ProtectedBundleState(c)(context.Background())
		require.NoError(t, err)

		_, ok := snap.ProtectedDigests[testDigest]
		assert.True(t, ok, "digest from agent status must be protected")
		_, ok = snap.ActiveAgents["my-agent"]
		assert.True(t, ok, "agent name must be in active set")
	})

	t.Run("deduplicates across sources", func(t *testing.T) {
		wo := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{Name: "wo-1"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "default"},
				ReplacePod: &v1alpha1.PodReplacement{
					Name:  "kit-agent",
					Image: "img",
					Env: []corev1.EnvVar{
						{Name: "KIT_AGENT_BUNDLE_SHA256", Value: testDigest},
					},
				},
			},
		}
		ag := &v1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "my-agent"},
			Spec: v1alpha1.AgentSpec{
				Script: v1alpha1.AgentScriptRef{Collection: "c", Package: "p"},
				Target: v1alpha1.AgentTarget{Namespace: "default"},
			},
			Status: v1alpha1.AgentStatus{
				BundleDigest: testDigest,
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wo, ag).Build()
		snap, err := agentctl.ProtectedBundleState(c)(context.Background())
		require.NoError(t, err)

		assert.Len(t, snap.ProtectedDigests, 1, "same digest from WO and Agent should dedup")
		assert.Len(t, snap.ActiveAgents, 1)
	})

	t.Run("multiple digests from multiple WOs", func(t *testing.T) {
		wo1 := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{Name: "wo-1"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "ns-1"},
				ReplacePod: &v1alpha1.PodReplacement{
					Name: "kit-agent", Image: "img",
					Env: []corev1.EnvVar{{Name: "KIT_AGENT_BUNDLE_SHA256", Value: testDigest}},
				},
			},
		}
		wo2 := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{Name: "wo-2"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "ns-2"},
				ReplacePod: &v1alpha1.PodReplacement{
					Name: "kit-agent", Image: "img",
					Env: []corev1.EnvVar{{Name: "KIT_AGENT_BUNDLE_SHA256", Value: testDigest2}},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wo1, wo2).Build()
		snap, err := agentctl.ProtectedBundleState(c)(context.Background())
		require.NoError(t, err)

		assert.Len(t, snap.ProtectedDigests, 2)
		_, ok1 := snap.ProtectedDigests[testDigest]
		_, ok2 := snap.ProtectedDigests[testDigest2]
		assert.True(t, ok1)
		assert.True(t, ok2)
	})

	t.Run("ignores invalid digest values", func(t *testing.T) {
		wo := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{Name: "wo-1"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "default"},
				ReplacePod: &v1alpha1.PodReplacement{
					Name: "kit-agent", Image: "img",
					Env: []corev1.EnvVar{
						{Name: "KIT_AGENT_BUNDLE_SHA256", Value: "not-a-digest"},
					},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wo).Build()
		snap, err := agentctl.ProtectedBundleState(c)(context.Background())
		require.NoError(t, err)

		assert.Empty(t, snap.ProtectedDigests, "invalid digest must be ignored")
	})

	t.Run("empty cluster returns empty snapshot", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
		snap, err := agentctl.ProtectedBundleState(c)(context.Background())
		require.NoError(t, err)
		assert.Empty(t, snap.ProtectedDigests)
		assert.Empty(t, snap.ActiveAgents)
	})
}
