package webhook_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	agentctl "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent"
	kitwebhook "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/webhook"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type fakeScriptLoader struct {
	packages []collection.ScriptPackageContent
	err      error
}

func (f *fakeScriptLoader) Load(_ context.Context, _ string, _ collection.ScriptCollectionResolver) (*collection.LoadResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	root := make(map[string]bool, len(f.packages))
	for _, p := range f.packages {
		root[p.Name] = true
	}
	return &collection.LoadResult{
		Packages:      f.packages,
		LocalPackages: root,
		ContentHash:   "fake",
	}, nil
}

func defaultFakeLoader() *fakeScriptLoader {
	return &fakeScriptLoader{
		packages: []collection.ScriptPackageContent{{
			Name:       "test-pkg",
			Entrypoint: "main.js",
			Files: []collection.ScriptFile{
				{Path: "main.js", Content: []byte(`exports.main = function(ctx) { return {exitCode: 0}; };`)},
			},
		}},
	}
}

func failingLoader() *fakeScriptLoader {
	return &fakeScriptLoader{err: fmt.Errorf("loader error")}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, rbacv1.AddToScheme(s))
	return s
}

func testStore(t *testing.T) *bundle.BundleStore {
	t.Helper()
	store, err := bundle.NewBundleStore(afero.NewMemMapFs(), "/bundles", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	return store
}

func testAgent() *v1alpha1.Agent {
	return &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mcd-stub",
			UID:  types.UID("agent-uid"),
		},
		Spec: v1alpha1.AgentSpec{
			Target: v1alpha1.AgentTarget{
				Namespace: "target-ns",
				Replace:   v1alpha1.AgentReplacePod,
				Labels:    map[string]string{"app": "mcd"},
			},
			Runtime: v1alpha1.AgentRuntimeJS,
			Script: v1alpha1.AgentScriptRef{
				Collection: "test-collection",
				Package:    "test-pkg",
			},
		},
	}
}

func trapMaterializer(t *testing.T, c client.Client) *agentctl.Materializer {
	t.Helper()
	return agentctl.NewMaterializer(c, testStore(t), failingLoader(), "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))
}

func admissionRequest(t *testing.T, pod *corev1.Pod, namespace string, dryRun bool) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	require.NoError(t, err)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("req-uid"),
			Namespace: namespace,
			Object:    runtime.RawExtension{Raw: raw},
			DryRun:    ptr.To(dryRun),
		},
	}
}

func TestPodMutator(t *testing.T) {
	t.Run("agent matching pod materializes WO on admission", func(t *testing.T) {
		scheme := testScheme(t)
		ag := testAgent()
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ag).
			Build()
		mat := agentctl.NewMaterializer(c, testStore(t), defaultFakeLoader(), "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))

		mutator := &kitwebhook.PodMutator{
			Client:       c,
			Materializer: mat,
			Decoder:      admission.NewDecoder(scheme),
			Log:          slog.New(slog.DiscardHandler),
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "mcd-pod",
				Labels: map[string]string{"app": "mcd"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "mcd", Image: "mcd:latest"}},
			},
		}

		resp := mutator.Handle(context.Background(), admissionRequest(t, pod, "target-ns", false))
		require.True(t, resp.Allowed, "response: %+v", resp.Result)
		assert.NotEmpty(t, resp.Patches, "pod must be mutated by the materialized WO")

		var wo v1alpha1.WorkloadOverride
		err := c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &wo)
		require.NoError(t, err, "WO must exist after webhook materialization")
	})

	t.Run("fresh WO skips materialization", func(t *testing.T) {
		const digest = "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"
		scheme := testScheme(t)
		ag := testAgent()
		ag.Generation = 1
		ag.Status = v1alpha1.AgentStatus{
			ObservedGeneration: 1,
			BundleDigest:       digest,
			Conditions: []metav1.Condition{{
				Type:               v1alpha1.ConditionAgentReady,
				Status:             metav1.ConditionTrue,
				Reason:             v1alpha1.ReasonAgentReady,
				ObservedGeneration: 1,
			}},
		}
		wo := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kit-agent-mcd-stub",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: v1alpha1.GroupVersion.String(),
					Kind:       "Agent",
					Name:       ag.Name,
					UID:        ag.UID,
				}},
			},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{
					Namespace: "target-ns",
					Labels:    map[string]string{"app": "mcd"},
				},
				ReplacePod: &v1alpha1.PodReplacement{
					Name:  "kit-agent",
					Image: "quay.io/kit:latest",
					Env: []corev1.EnvVar{
						{Name: "KIT_AGENT_BUNDLE_SHA256", Value: digest},
					},
				},
			},
		}
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ag, wo).
			Build()

		mutator := &kitwebhook.PodMutator{
			Client:       c,
			Materializer: trapMaterializer(t, c),
			Decoder:      admission.NewDecoder(scheme),
			Log:          slog.New(slog.DiscardHandler),
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "mcd-pod",
				Labels: map[string]string{"app": "mcd"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "mcd", Image: "mcd:latest"}},
			},
		}

		resp := mutator.Handle(context.Background(), admissionRequest(t, pod, "target-ns", false))
		require.True(t, resp.Allowed, "fresh WO must not trigger materialization failure")
		assert.NotEmpty(t, resp.Patches, "fresh WO must still mutate the pod")

		var after v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &after))
		assert.Equal(t, digest, agentctl.WODigest(&after), "WO must not be modified")
	})

	t.Run("stale WO triggers re-materialization", func(t *testing.T) {
		scheme := testScheme(t)
		ag := testAgent()
		ag.Generation = 2
		ag.Status = v1alpha1.AgentStatus{
			ObservedGeneration: 1,
			BundleDigest:       "0000000000000000000000000000000000000000000000000000000000000000",
		}
		wo := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kit-agent-mcd-stub",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: v1alpha1.GroupVersion.String(),
					Kind:       "Agent",
					Name:       ag.Name,
					UID:        ag.UID,
				}},
			},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{
					Namespace: "target-ns",
					Labels:    map[string]string{"app": "mcd"},
				},
				ReplacePod: &v1alpha1.PodReplacement{
					Name:  "kit-agent",
					Image: "quay.io/kit:latest",
					Env: []corev1.EnvVar{
						{Name: "KIT_AGENT_BUNDLE_SHA256", Value: "0000000000000000000000000000000000000000000000000000000000000000"},
					},
				},
			},
		}
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ag, wo).
			Build()
		mat := agentctl.NewMaterializer(c, testStore(t), defaultFakeLoader(), "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))

		mutator := &kitwebhook.PodMutator{
			Client:       c,
			Materializer: mat,
			Decoder:      admission.NewDecoder(scheme),
			Log:          slog.New(slog.DiscardHandler),
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "mcd-pod",
				Labels: map[string]string{"app": "mcd"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "mcd", Image: "mcd:latest"}},
			},
		}

		resp := mutator.Handle(context.Background(), admissionRequest(t, pod, "target-ns", false))
		require.True(t, resp.Allowed, "response: %+v", resp.Result)

		var updated v1alpha1.WorkloadOverride
		err := c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &updated)
		require.NoError(t, err)
		newDigest := ""
		if updated.Spec.ReplacePod != nil {
			for _, env := range updated.Spec.ReplacePod.Env {
				if env.Name == "KIT_AGENT_BUNDLE_SHA256" {
					newDigest = env.Value
				}
			}
		}
		assert.NotEqual(t, "0000000000000000000000000000000000000000000000000000000000000000", newDigest,
			"WO must be updated with the new bundle digest")
	})

	t.Run("unowned WO triggers materialization", func(t *testing.T) {
		scheme := testScheme(t)
		ag := testAgent()
		ag.Generation = 1
		ag.Status = v1alpha1.AgentStatus{
			ObservedGeneration: 1,
			BundleDigest:       "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234",
		}
		wo := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{Name: "kit-agent-mcd-stub"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{
					Namespace: "target-ns",
					Labels:    map[string]string{"app": "mcd"},
				},
				ReplacePod: &v1alpha1.PodReplacement{
					Name:  "kit-agent",
					Image: "quay.io/kit:latest",
					Env: []corev1.EnvVar{
						{Name: "KIT_AGENT_BUNDLE_SHA256", Value: "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"},
					},
				},
			},
		}
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ag, wo).
			Build()
		mat := agentctl.NewMaterializer(c, testStore(t), defaultFakeLoader(), "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))

		mutator := &kitwebhook.PodMutator{
			Client:       c,
			Materializer: mat,
			Decoder:      admission.NewDecoder(scheme),
			Log:          slog.New(slog.DiscardHandler),
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "mcd-pod",
				Labels: map[string]string{"app": "mcd"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "mcd", Image: "mcd:latest"}},
			},
		}

		resp := mutator.Handle(context.Background(), admissionRequest(t, pod, "target-ns", false))
		require.True(t, resp.Allowed, "response: %+v", resp.Result)

		var updated v1alpha1.WorkloadOverride
		err := c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &updated)
		require.NoError(t, err)
		assert.NotEmpty(t, updated.OwnerReferences, "WO must have owner references after re-materialization")
	})

	t.Run("dry run skips materialization", func(t *testing.T) {
		scheme := testScheme(t)
		ag := testAgent()
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ag).
			Build()

		mutator := &kitwebhook.PodMutator{
			Client:       c,
			Materializer: trapMaterializer(t, c),
			Decoder:      admission.NewDecoder(scheme),
			Log:          slog.New(slog.DiscardHandler),
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "mcd-pod",
				Labels: map[string]string{"app": "mcd"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "mcd", Image: "mcd:latest"}},
			},
		}

		resp := mutator.Handle(context.Background(), admissionRequest(t, pod, "target-ns", true))
		assert.True(t, resp.Allowed, "dryRun must not trigger materialization")
	})

	t.Run("non-matching pod passes through", func(t *testing.T) {
		scheme := testScheme(t)
		ag := testAgent()
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ag).
			Build()

		mutator := &kitwebhook.PodMutator{
			Client:       c,
			Materializer: trapMaterializer(t, c),
			Decoder:      admission.NewDecoder(scheme),
			Log:          slog.New(slog.DiscardHandler),
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "other-pod",
				Labels: map[string]string{"app": "other"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "main", Image: "img"}},
			},
		}

		resp := mutator.Handle(context.Background(), admissionRequest(t, pod, "other-ns", false))
		assert.True(t, resp.Allowed)
		assert.Empty(t, resp.Patches, "non-matching pod must not be mutated")
	})

	t.Run("no agents no materialization", func(t *testing.T) {
		scheme := testScheme(t)
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		mutator := &kitwebhook.PodMutator{
			Client:       c,
			Materializer: trapMaterializer(t, c),
			Decoder:      admission.NewDecoder(scheme),
			Log:          slog.New(slog.DiscardHandler),
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "some-pod"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "main", Image: "img"}},
			},
		}

		resp := mutator.Handle(context.Background(), admissionRequest(t, pod, "default", false))
		assert.True(t, resp.Allowed)
		assert.Empty(t, resp.Patches)
	})

	t.Run("materialization failure rejects pod", func(t *testing.T) {
		scheme := testScheme(t)
		ag := testAgent()
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(ag).
			Build()
		mat := agentctl.NewMaterializer(c, testStore(t), failingLoader(), "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))

		mutator := &kitwebhook.PodMutator{
			Client:       c,
			Materializer: mat,
			Decoder:      admission.NewDecoder(scheme),
			Log:          slog.New(slog.DiscardHandler),
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "mcd-pod",
				Labels: map[string]string{"app": "mcd"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "mcd", Image: "mcd:latest"}},
			},
		}

		resp := mutator.Handle(context.Background(), admissionRequest(t, pod, "target-ns", false))
		assert.False(t, resp.Allowed, "pod must be rejected when materialization fails")
	})
}
