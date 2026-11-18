package agent_test

import (
	"context"
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
	agentrbac "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent/rbac"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type fakeScriptLoader struct {
	packages      []collection.ScriptPackageContent
	localPackages map[string]bool
	err           error
}

func (f *fakeScriptLoader) Load(_ context.Context, _ string, _ collection.ScriptCollectionResolver) (*collection.LoadResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	root := f.localPackages
	if root == nil {
		root = make(map[string]bool, len(f.packages))
		for _, p := range f.packages {
			root[p.Name] = true
		}
	}
	return &collection.LoadResult{
		Packages:      f.packages,
		LocalPackages: root,
		ContentHash:   "fake",
	}, nil
}

func defaultFakeLoader() *fakeScriptLoader {
	return fakeLoaderWithScript(`exports.main = function(ctx) { return {exitCode: 0}; };`)
}

func fakeLoaderWithScript(content string) *fakeScriptLoader {
	return &fakeScriptLoader{
		packages: []collection.ScriptPackageContent{{
			Name:       "test-pkg",
			Entrypoint: "main.js",
			Files: []collection.ScriptFile{
				{Path: "main.js", Content: []byte(content)},
			},
		}},
	}
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
			Runtime: v1alpha1.AgentRuntimeJS,
			Script: v1alpha1.AgentScriptRef{
				Collection: "test-collection",
				Package:    "test-pkg",
			},
		},
	}
}

func newMaterializer(t *testing.T, c client.Client, loader agentctl.ScriptPackageLoader) *agentctl.Materializer {
	t.Helper()
	return agentctl.NewMaterializer(c, testStore(t), loader, "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))
}

func TestMaterialize(t *testing.T) {
	t.Run("creates all resources", func(t *testing.T) {
		ag := testAgent("mcd-stub")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithStatusSubresource(&v1alpha1.Agent{}).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
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

		var cr rbacv1.ClusterRole
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &cr))
		assert.NotEmpty(t, cr.Rules)

		var crb rbacv1.ClusterRoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &crb))
		assert.Equal(t, "kit-agent-mcd-stub", crb.Subjects[0].Name)
		assert.Equal(t, "target-ns", crb.Subjects[0].Namespace)
		assert.Equal(t, "ClusterRole", crb.RoleRef.Kind)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-mcd-stub"}, &wo))
		assert.Equal(t, "target-ns", wo.Spec.Match.Namespace)
		assert.Equal(t, "kit-agent-mcd-stub", wo.Spec.ServiceAccountName)
	})

	t.Run("replace pod generates ReplacePod", func(t *testing.T) {
		ag := testAgent("test")
		ag.Spec.Target.Replace = v1alpha1.AgentReplacePod
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
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
		ag := testAgent("test")
		ag.Spec.Target.Replace = v1alpha1.AgentReplaceContainer
		ag.Spec.Target.Container = "mcd"
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
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
		ag := testAgent("my-agent")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		result, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-my-agent"}, &wo))
		require.NotNil(t, wo.Spec.ReplacePod)
		env := wo.Spec.ReplacePod.Env
		require.Len(t, env, 5)

		envByName := make(map[string]corev1.EnvVar, len(env))
		for _, e := range env {
			envByName[e.Name] = e
		}

		assert.Equal(t, fmt.Sprintf("http://controller:8082/bundles/%s", result.BundleDigest), envByName["KIT_AGENT_BUNDLE_URL"].Value)
		assert.Equal(t, result.BundleDigest, envByName["KIT_AGENT_BUNDLE_SHA256"].Value)

		assert.NotNil(t, envByName["KIT_POD_NAME"].ValueFrom)
		assert.Equal(t, "metadata.name", envByName["KIT_POD_NAME"].ValueFrom.FieldRef.FieldPath)
		assert.NotNil(t, envByName["KIT_POD_UID"].ValueFrom)
		assert.Equal(t, "metadata.uid", envByName["KIT_POD_UID"].ValueFrom.FieldRef.FieldPath)
		assert.NotNil(t, envByName["KIT_NODE_NAME"].ValueFrom)
		assert.Equal(t, "spec.nodeName", envByName["KIT_NODE_NAME"].ValueFrom.FieldRef.FieldPath)
	})

	t.Run("loader error propagates", func(t *testing.T) {
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		loader := &fakeScriptLoader{err: fmt.Errorf("collection not found")}
		mat := newMaterializer(t, c, loader)
		_, err := mat.Materialize(context.Background(), ag)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "collection not found")
	})

	t.Run("missing package returns error", func(t *testing.T) {
		ag := testAgent("test")
		ag.Spec.Script.Package = "nonexistent"
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not in collection")
	})

	t.Run("entrypoint from dependency rejected", func(t *testing.T) {
		ag := testAgent("test")
		ag.Spec.Script.Package = "dep-pkg"
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		loader := &fakeScriptLoader{
			packages: []collection.ScriptPackageContent{
				{
					Name:       "test-pkg",
					Entrypoint: "main.js",
					Files:      []collection.ScriptFile{{Path: "main.js", Content: []byte("main")}},
				},
				{
					Name:       "dep-pkg",
					Entrypoint: "main.js",
					Files:      []collection.ScriptFile{{Path: "main.js", Content: []byte("dep")}},
				},
			},
			localPackages: map[string]bool{"test-pkg": true},
		}
		mat := newMaterializer(t, c, loader)
		_, err := mat.Materialize(context.Background(), ag)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not in collection")
	})

	t.Run("idempotent second call", func(t *testing.T) {
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		r1, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		r2, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)
		assert.Equal(t, r1.BundleDigest, r2.BundleDigest)
	})

	t.Run("owner references on all resources", func(t *testing.T) {
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
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

		var cr rbacv1.ClusterRole
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &cr))
		checkOwner(&cr)

		var crb rbacv1.ClusterRoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &crb))
		checkOwner(&crb)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		checkOwner(&wo)
	})

	t.Run("match copies agent target fields", func(t *testing.T) {
		ag := testAgent("test")
		ag.Spec.Target.PodName = "specific-pod"
		ag.Spec.Target.NodeName = "node-1"
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		assert.Equal(t, "target-ns", wo.Spec.Match.Namespace)
		assert.Equal(t, map[string]string{"app": "mcd"}, wo.Spec.Match.Labels)
		assert.Equal(t, "specific-pod", wo.Spec.Match.PodName)
		assert.Equal(t, "node-1", wo.Spec.Match.NodeName)
	})

	t.Run("updates an existing generated cluster role", func(t *testing.T) {
		ag := testAgent("test")
		existing := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "kit-agent-test"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get"},
			}},
		}
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(existing).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var updated rbacv1.ClusterRole
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &updated))
		hasRule := func(group, resource, verb string) bool {
			for _, rule := range updated.Rules {
				for _, apiGroup := range rule.APIGroups {
					if apiGroup != group {
						continue
					}
					for _, ruleResource := range rule.Resources {
						if ruleResource != resource {
							continue
						}
						for _, ruleVerb := range rule.Verbs {
							if ruleVerb == verb {
								return true
							}
						}
					}
				}
			}
			return false
		}
		assert.True(t, hasRule("", "nodes", "patch"), "SSA should update stale ClusterRole")
		assert.True(t, hasRule("machineconfiguration.openshift.io", "machineconfignodes", "patch"))
	})

	t.Run("WO update adopts owner references", func(t *testing.T) {
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
			WithObjects(orphanWO).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var wo v1alpha1.WorkloadOverride
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &wo))
		require.Len(t, wo.OwnerReferences, 1)
		assert.Equal(t, ag.UID, wo.OwnerReferences[0].UID)
	})

	t.Run("WO updated when digest changes", func(t *testing.T) {
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		store := testStore(t)
		loader := defaultFakeLoader()
		mat := agentctl.NewMaterializer(c, store, loader, "quay.io/kit:latest", "http://controller:8082", slog.New(slog.DiscardHandler))
		r1, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		loader.packages[0].Files[0].Content = []byte(`exports.main = function(ctx) { return {exitCode: 1}; };`)
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

	t.Run("labels use agent UID on all generated objects", func(t *testing.T) {
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		checkLabel := func(obj client.Object) {
			t.Helper()
			assert.Equal(t, string(ag.UID), obj.GetLabels()[agentrbac.LabelAgentUID],
				"%T should have agent-uid label", obj)
		}

		var sa corev1.ServiceAccount
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &sa))
		checkLabel(&sa)

		var role rbacv1.Role
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &role))
		checkLabel(&role)

		var rb rbacv1.RoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &rb))
		checkLabel(&rb)

		var cr rbacv1.ClusterRole
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &cr))
		checkLabel(&cr)

		var crb rbacv1.ClusterRoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &crb))
		checkLabel(&crb)
	})

	t.Run("namespace retarget repairs subjects and prunes old objects", func(t *testing.T) {
		ag := testAgent("test")
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		// Verify initial state in target-ns.
		var sa corev1.ServiceAccount
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &sa))
		var role rbacv1.Role
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &role))

		// Retarget to a different namespace.
		ag.Spec.Target.Namespace = "new-ns"
		_, err = mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		// New objects exist in new-ns.
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "new-ns"}, &sa))
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "new-ns"}, &role))

		var rb rbacv1.RoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "new-ns"}, &rb))
		assert.Equal(t, "new-ns", rb.Subjects[0].Namespace)

		// ClusterRoleBinding subject updated to new namespace.
		var crb rbacv1.ClusterRoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &crb))
		assert.Equal(t, "new-ns", crb.Subjects[0].Namespace)

		// Old namespaced objects pruned.
		err = c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &sa)
		assert.True(t, err != nil, "old SA should be pruned")
		err = c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &role)
		assert.True(t, err != nil, "old Role should be pruned")
		err = c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &rb)
		assert.True(t, err != nil, "old RoleBinding should be pruned")
	})

	t.Run("stale CRB subject repaired", func(t *testing.T) {
		ag := testAgent("test")
		staleCRB := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "kit-agent-test",
				Labels: map[string]string{agentrbac.LabelAgentUID: string(ag.UID)},
			},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      "kit-agent-test",
				Namespace: "wrong-namespace",
			}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     "kit-agent-test",
			},
		}
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(staleCRB).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var crb rbacv1.ClusterRoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test"}, &crb))
		require.Len(t, crb.Subjects, 1)
		assert.Equal(t, "target-ns", crb.Subjects[0].Namespace,
			"SSA should repair stale subject namespace")
	})

	t.Run("stale role rules repaired", func(t *testing.T) {
		ag := testAgent("test")
		staleRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kit-agent-test",
				Namespace: "target-ns",
				Labels:    map[string]string{agentrbac.LabelAgentUID: string(ag.UID)},
			},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get"},
			}},
		}
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(staleRole).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var role rbacv1.Role
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &role))
		for _, rule := range role.Rules {
			for _, res := range rule.Resources {
				assert.NotEqual(t, "secrets", res,
					"SSA should replace stale rules, secrets should not remain")
			}
		}
		assert.NotEmpty(t, role.Rules, "role should have the correct rules after repair")
	})

	t.Run("user object with agent label but no owner ref not deleted", func(t *testing.T) {
		ag := testAgent("test")
		userCRB := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "user-managed-crb",
				Labels: map[string]string{agentrbac.LabelAgentUID: string(ag.UID)},
			},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      "user-sa",
				Namespace: "user-ns",
			}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     "user-role",
			},
		}
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(userCRB).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		var crb rbacv1.ClusterRoleBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "user-managed-crb"}, &crb),
			"user-managed CRB without owner ref should survive prune")
	})

	t.Run("WO failure after retarget preserves old objects", func(t *testing.T) {
		ag := testAgent("test")
		failWO := false
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if failWO {
						if _, ok := obj.(*v1alpha1.WorkloadOverride); ok {
							return fmt.Errorf("simulated WO failure")
						}
					}
					return cl.Patch(ctx, obj, patch, opts...)
				},
			}).
			Build()

		mat := newMaterializer(t, c, defaultFakeLoader())
		_, err := mat.Materialize(context.Background(), ag)
		require.NoError(t, err)

		// Enable WO failure and retarget.
		failWO = true
		ag.Spec.Target.Namespace = "new-ns"
		_, err = mat.Materialize(context.Background(), ag)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "simulated WO failure")

		// Old SA and Role in target-ns must still exist.
		var sa corev1.ServiceAccount
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &sa),
			"old SA should survive when WO fails")
		var role rbacv1.Role
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kit-agent-test", Namespace: "target-ns"}, &role),
			"old Role should survive when WO fails")
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
