package kube_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/kube"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestClientPolicyEnforcement(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper(nil)
	c := kube.NewClient(nil, mapper, kube.NewPolicy())

	ctx := context.Background()
	secretsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

	_, err := c.Get(ctx, secretsGVR, "default", "my-secret")
	assert.ErrorContains(t, err, "denied by agent policy")

	_, err = c.List(ctx, secretsGVR, "default", metav1.ListOptions{})
	assert.ErrorContains(t, err, "denied by agent policy")

	kitGVR := schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "agents"}
	_, err = c.Create(ctx, kitGVR, nil)
	assert.ErrorContains(t, err, "denied by agent policy")

	err = c.Delete(ctx, kitGVR, "default", "my-agent")
	assert.ErrorContains(t, err, "denied by agent policy")
}

func TestClientResolveGVR(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "apps", Version: "v1"},
	})
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)

	c := kube.NewClient(nil, mapper, kube.NewPolicy())

	gvr, err := c.ResolveGVR(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	require.NoError(t, err)
	assert.Equal(t, "deployments", gvr.Resource)
	assert.Equal(t, "apps", gvr.Group)
	assert.Equal(t, "v1", gvr.Version)
}

func TestClientResolveGVRNotFound(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper(nil)
	c := kube.NewClient(nil, mapper, kube.NewPolicy())

	_, err := c.ResolveGVR(schema.GroupVersionKind{Group: "nonexistent", Version: "v1", Kind: "Foo"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "resolving")
}
