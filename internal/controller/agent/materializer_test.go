package agent_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	agentctl "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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

func writeCollectionDir(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	collDir := filepath.Join(dir, "test-collection", "scripts", "test-pkg")
	require.NoError(t, os.MkdirAll(collDir, 0755))
	for name, content := range scripts {
		full := filepath.Join(collDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0644))
	}
	return dir
}

func testAgent(name string) *v1alpha1.Agent {
	return &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("test-uid"),
		},
		Spec: v1alpha1.AgentSpec{
			Target: v1alpha1.AgentTarget{
				Namespace: "target-ns",
				Replace:   v1alpha1.AgentReplacePod,
				Labels:    map[string]string{"app": "mcd"},
			},
			Runtime:   v1alpha1.AgentRuntimeJS,
			Lifecycle: v1alpha1.AgentLifecycleBounded,
			Script: v1alpha1.AgentScriptRef{
				Collection: "test-collection",
				Package:    "test-pkg",
			},
		},
	}
}

func testCollection() *v1alpha1.ResourceCollection {
	return &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{Name: "test-collection"},
		Spec: v1alpha1.ResourceCollectionSpec{
			Source: "test-collection",
			Scripts: []v1alpha1.ScriptPackage{{
				Name:       "test-pkg",
				Root:       "scripts/test-pkg",
				Entrypoint: "main.js",
			}},
		},
	}
}

func newMaterializer(t *testing.T, c client.Client, collectionsDir string) *agentctl.Materializer {
	t.Helper()
	return agentctl.NewMaterializer(c, testStore(t), collectionsDir, "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))
}

func TestMaterialize(t *testing.T) {
	t.Run("creates all resources", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) { return {exitCode: 0}; };`,
		})
		ag := testAgent("mcd-stub")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			WithStatusSubresource(&v1alpha1.Agent{}).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		result, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)
		assert.NotEmpty(t, result.BundleDigest)
		assert.Len(t, result.BundleDigest, 64)

		var sa corev1.ServiceAccount
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub", Namespace: "target-ns"}, &sa))
		assert.Equal(t, "Agent", sa.OwnerReferences[0].Kind)

		var role rbacv1.Role
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub", Namespace: "target-ns"}, &role))
		assert.NotEmpty(t, role.Rules)

		var rb rbacv1.RoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub", Namespace: "target-ns"}, &rb))
		assert.Equal(t, "kit-agent-mcd-stub", rb.Subjects[0].Name)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &wo))
		assert.Equal(t, "target-ns", wo.Spec.Match.Namespace)
		assert.Equal(t, "kit-agent-mcd-stub", wo.Spec.ServiceAccountName)
	})

	t.Run("replace pod generates ReplacePod", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("test")
		ag.Spec.Target.Replace = v1alpha1.AgentReplacePod
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		require.NotNil(t, wo.Spec.ReplacePod)
		assert.Equal(t, "kit-agent", wo.Spec.ReplacePod.Name)
		assert.Equal(t, "quay.io/kit:latest", wo.Spec.ReplacePod.Image)
		assert.Equal(t, []string{"/usr/bin/kit-agent"}, wo.Spec.ReplacePod.Command)
		assert.Empty(t, wo.Spec.Containers)
	})

	t.Run("replace container generates ContainerOverride", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("test")
		ag.Spec.Target.Replace = v1alpha1.AgentReplaceContainer
		ag.Spec.Target.Container = "mcd"
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		assert.Nil(t, wo.Spec.ReplacePod)
		require.Len(t, wo.Spec.Containers, 1)
		assert.Equal(t, "mcd", wo.Spec.Containers[0].Name)
		assert.Equal(t, "quay.io/kit:latest", wo.Spec.Containers[0].Image)
	})

	t.Run("bundle env vars populated", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("my-agent")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		result, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-my-agent"}, &wo))
		require.NotNil(t, wo.Spec.ReplacePod)
		env := wo.Spec.ReplacePod.Env
		require.Len(t, env, 2)
		assert.Equal(t, "KIT_AGENT_BUNDLE_URL", env[0].Name)
		assert.Equal(t, fmt.Sprintf("http://controller:8082/bundles/%s", result.BundleDigest), env[0].Value)
		assert.Equal(t, "KIT_AGENT_BUNDLE_SHA256", env[1].Name)
		assert.Equal(t, result.BundleDigest, env[1].Value)
	})

	t.Run("missing collection returns error", func(t *testing.T) {
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, t.TempDir())
		_, err := mat.Materialize(context.Background(), ag)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading collection")
	})

	t.Run("missing package returns error", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("test")
		ag.Spec.Script.Package = "nonexistent"
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		_, err := mat.Materialize(context.Background(), ag)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in collection")
	})

	t.Run("idempotent second call", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		r1, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		r2, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)
		assert.Equal(t, r1.BundleDigest, r2.BundleDigest)
	})

	t.Run("owner references on all resources", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		checkOwner := func(obj client.Object) {
			t.Helper()
			refs := obj.GetOwnerReferences()
			require.Len(t, refs, 1)
			assert.Equal(t, "Agent", refs[0].Kind)
			assert.Equal(t, ag.Name, refs[0].Name)
			assert.Equal(t, ag.UID, refs[0].UID)
		}

		var sa corev1.ServiceAccount
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &sa))
		checkOwner(&sa)

		var role rbacv1.Role
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &role))
		checkOwner(&role)

		var rb rbacv1.RoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &rb))
		checkOwner(&rb)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		checkOwner(&wo)
	})

	t.Run("match copies agent target fields", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("test")
		ag.Spec.Target.PodName = "specific-pod"
		ag.Spec.Target.NodeName = "node-1"
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		assert.Equal(t, "target-ns", wo.Spec.Match.Namespace)
		assert.Equal(t, map[string]string{"app": "mcd"}, wo.Spec.Match.Labels)
		assert.Equal(t, "specific-pod", wo.Spec.Match.PodName)
		assert.Equal(t, "node-1", wo.Spec.Match.NodeName)
	})

	t.Run("role excludes secrets", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var role rbacv1.Role
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &role))
		for _, rule := range role.Rules {
			for _, res := range rule.Resources {
				assert.NotEqual(t, "secrets", res)
			}
		}
	})

	t.Run("WO update adopts owner references", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) {};`,
		})
		ag := testAgent("test")
		scheme := testScheme(t)
		orphanWO := &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{Name: "kit-agent-test"},
			Spec: v1alpha1.WorkloadOverrideSpec{
				Match: v1alpha1.WorkloadOverrideMatch{Namespace: "target-ns"},
			},
		}
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testCollection(), orphanWO).
			Build()

		mat := newMaterializer(t, c, collectionsDir)
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		require.Len(t, wo.OwnerReferences, 1)
		assert.Equal(t, ag.UID, wo.OwnerReferences[0].UID)
	})

	t.Run("WO updated when digest changes", func(t *testing.T) {
		collectionsDir := writeCollectionDir(t, map[string]string{
			"main.js": `exports.main = function(ctx) { return {exitCode: 0}; };`,
		})
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(testCollection()).
			Build()

		store := testStore(t)
		mat := agentctl.NewMaterializer(c, store, collectionsDir, "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))
		r1, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		// Change the script content
		scriptPath := filepath.Join(collectionsDir, "test-collection", "scripts", "test-pkg", "main.js")
		require.NoError(t, os.WriteFile(scriptPath, []byte(`exports.main = function(ctx) { return {exitCode: 1}; };`), 0644))

		r2, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)
		assert.NotEqual(t, r1.BundleDigest, r2.BundleDigest)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		require.NotNil(t, wo.Spec.ReplacePod)
		var digestEnv string
		for _, env := range wo.Spec.ReplacePod.Env {
			if env.Name == "KIT_AGENT_BUNDLE_SHA256" {
				digestEnv = env.Value
			}
		}
		assert.Equal(t, r2.BundleDigest, digestEnv)
	})
}

func TestGeneratedName(t *testing.T) {
	assert.Equal(t, "kit-agent-mcd-stub", agentctl.GeneratedName("mcd-stub"))
	assert.Equal(t, "kit-agent-x", agentctl.GeneratedName("x"))
}

func TestIsWOFresh(t *testing.T) {
	const digest = "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"
	const otherDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	agentUID := types.UID("agent-uid-1")

	baseAgent := func() *v1alpha1.Agent {
		ag := testAgent("test")
		ag.UID = agentUID
		ag.Generation = 3
		ag.Status = v1alpha1.AgentStatus{
			ObservedGeneration: 3,
			BundleDigest:       digest,
			Conditions: []metav1.Condition{{
				Type:               v1alpha1.ConditionAgentReady,
				Status:             metav1.ConditionTrue,
				Reason:             v1alpha1.ReasonAgentReady,
				ObservedGeneration: 3,
			}},
		}
		return ag
	}

	baseWO := func() *v1alpha1.WorkloadOverride {
		return &v1alpha1.WorkloadOverride{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kit-agent-test",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: v1alpha1.GroupVersion.String(),
					Kind:       "Agent",
					Name:       "test",
					UID:        agentUID,
				}},
			},
			Spec: v1alpha1.WorkloadOverrideSpec{
				ReplacePod: &v1alpha1.PodReplacement{
					Env: []corev1.EnvVar{
						{Name: "KIT_AGENT_BUNDLE_SHA256", Value: digest},
					},
				},
			},
		}
	}

	cases := []struct {
		name  string
		mutAg func(*v1alpha1.Agent)
		mutWO func(*v1alpha1.WorkloadOverride)
		want  bool
	}{
		{
			name: "all checks pass",
			want: true,
		},
		{
			name: "digest in ContainerOverride",
			mutWO: func(wo *v1alpha1.WorkloadOverride) {
				wo.Spec.ReplacePod = nil
				wo.Spec.Containers = []v1alpha1.ContainerOverride{{
					Name: "mcd",
					Env:  []v1alpha1.EnvOverride{{Name: "KIT_AGENT_BUNDLE_SHA256", Value: digest}},
				}}
			},
			want: true,
		},
		{
			name:  "wrong owner UID",
			mutWO: func(wo *v1alpha1.WorkloadOverride) { wo.OwnerReferences[0].UID = "other-uid" },
			want:  false,
		},
		{
			name:  "no owner references",
			mutWO: func(wo *v1alpha1.WorkloadOverride) { wo.OwnerReferences = nil },
			want:  false,
		},
		{
			name:  "generation mismatch",
			mutAg: func(ag *v1alpha1.Agent) { ag.Generation = 4 },
			want:  false,
		},
		{
			name: "ready condition false",
			mutAg: func(ag *v1alpha1.Agent) {
				ag.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			want: false,
		},
		{
			name: "ready condition stale generation",
			mutAg: func(ag *v1alpha1.Agent) {
				ag.Status.Conditions[0].ObservedGeneration = 2
			},
			want: false,
		},
		{
			name:  "no conditions",
			mutAg: func(ag *v1alpha1.Agent) { ag.Status.Conditions = nil },
			want:  false,
		},
		{
			name:  "empty bundle digest in status",
			mutAg: func(ag *v1alpha1.Agent) { ag.Status.BundleDigest = "" },
			want:  false,
		},
		{
			name:  "digest mismatch",
			mutWO: func(wo *v1alpha1.WorkloadOverride) { wo.Spec.ReplacePod.Env[0].Value = otherDigest },
			want:  false,
		},
		{
			name:  "no digest env var",
			mutWO: func(wo *v1alpha1.WorkloadOverride) { wo.Spec.ReplacePod.Env = nil },
			want:  false,
		},
		{
			name: "empty WO spec",
			mutWO: func(wo *v1alpha1.WorkloadOverride) {
				wo.Spec.ReplacePod = nil
				wo.Spec.Containers = nil
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ag := baseAgent()
			wo := baseWO()
			if tc.mutAg != nil {
				tc.mutAg(ag)
			}
			if tc.mutWO != nil {
				tc.mutWO(wo)
			}
			assert.Equal(t, tc.want, agentctl.IsWOFresh(ag, wo))
		})
	}
}
