package kube_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func allowConfigMapsInDefault() *kube.Policy {
	return kube.NewPolicy("default", []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "list"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "create"},
	})
}

func TestClientPolicyEnforcement(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper(nil)
	c := kube.NewClient(nil, mapper, allowConfigMapsInDefault())

	ctx := context.Background()
	secretsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

	// Secrets not in capabilities — denied regardless of namespace.
	_, err := c.Get(ctx, secretsGVR, "default", "my-secret")
	assert.ErrorContains(t, err, "missing capability")

	_, err = c.List(ctx, secretsGVR, "default", metav1.ListOptions{})
	assert.ErrorContains(t, err, "missing capability")

	// Kit resources not in capabilities — denied.
	kitGVR := schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "agents"}
	_, err = c.Create(ctx, kitGVR, nil)
	assert.ErrorContains(t, err, "missing capability")

	err = c.Delete(ctx, kitGVR, "default", "my-agent")
	assert.ErrorContains(t, err, "missing capability")

	// Allowed resource in the right namespace passes — verify directly
	// via policy so we don't need a real dynamic client.
	cmsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	policy := allowConfigMapsInDefault()
	assert.NoError(t, policy.CheckAccess(kube.AccessRequest{GVR: cmsGVR, Verb: "get", Namespace: "default"}))
	assert.ErrorContains(t, policy.CheckAccess(kube.AccessRequest{GVR: cmsGVR, Verb: "get", Namespace: "other-ns"}), "missing capability")
}

func TestClientResolveGVR(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "apps", Version: "v1"},
	})
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)

	c := kube.NewClient(nil, mapper, kube.NewPolicy("", nil))

	gvr, err := c.ResolveGVR(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	require.NoError(t, err)
	assert.Equal(t, "deployments", gvr.Resource)
	assert.Equal(t, "apps", gvr.Group)
	assert.Equal(t, "v1", gvr.Version)
}

func TestClientResolveGVRNotFound(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper(nil)
	c := kube.NewClient(nil, mapper, kube.NewPolicy("", nil))

	_, err := c.ResolveGVR(schema.GroupVersionKind{Group: "nonexistent", Version: "v1", Kind: "Foo"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "resolving")
}
