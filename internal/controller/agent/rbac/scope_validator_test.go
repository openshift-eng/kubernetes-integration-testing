package rbac_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent/rbac"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	discoveryfake "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testDiscovery(resourceLists ...*metav1.APIResourceList) *discoveryfake.FakeDiscovery {
	return &discoveryfake.FakeDiscovery{
		Fake: &k8stesting.Fake{Resources: resourceLists},
	}
}

func atomicCapability(scope v1alpha1.ScriptCapabilityScope, group, resource string) collection.AtomicCapability {
	return collection.AtomicCapability{
		Scope:    scope,
		APIGroup: group,
		Resource: resource,
		Verb:     "get",
	}
}

func TestScopeValidator(t *testing.T) {
	discoveryClient := testDiscovery(
		&metav1.APIResourceList{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Namespaced: true},
				{Name: "nodes", Namespaced: false},
			},
		},
		&metav1.APIResourceList{
			GroupVersion: "example.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "widgets", Namespaced: true},
				{Name: "widgets/status", Namespaced: true},
			},
		},
	)
	validator := rbac.NewScopeValidator(discoveryClient)

	tests := []struct {
		name    string
		cap     collection.AtomicCapability
		wantErr string
	}{
		{
			name: "target namespace accepts namespaced resource",
			cap:  atomicCapability(v1alpha1.ScopeTargetNamespace, "", "pods"),
		},
		{
			name: "all namespaces accepts namespaced resource",
			cap:  atomicCapability(v1alpha1.ScopeAllNamespaces, "example.io", "widgets"),
		},
		{
			name: "cluster accepts cluster-scoped resource",
			cap:  atomicCapability(v1alpha1.ScopeCluster, "", "nodes"),
		},
		{
			name: "subresource is validated exactly",
			cap:  atomicCapability(v1alpha1.ScopeTargetNamespace, "example.io", "widgets/status"),
		},
		{
			name:    "cluster rejects namespaced resource",
			cap:     atomicCapability(v1alpha1.ScopeCluster, "", "pods"),
			wantErr: "invalid",
		},
		{
			name:    "target namespace rejects cluster-scoped resource",
			cap:     atomicCapability(v1alpha1.ScopeTargetNamespace, "", "nodes"),
			wantErr: "invalid",
		},
		{
			name:    "missing resource is pending",
			cap:     atomicCapability(v1alpha1.ScopeCluster, "example.io", "missing"),
			wantErr: "pending",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validator.Validate(context.Background(), []collection.AtomicCapability{tc.cap})
			switch tc.wantErr {
			case "":
				require.NoError(t, err)
			case "invalid":
				var target *rbac.InvalidCapabilitiesError
				require.ErrorAs(t, err, &target)
			case "pending":
				var target *rbac.APITypePendingError
				require.ErrorAs(t, err, &target)
			}
		})
	}
}

func TestScopeValidatorRetriesDiscoveryErrors(t *testing.T) {
	discoveryClient := testDiscovery(&metav1.APIResourceList{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{Name: "nodes", Namespaced: false}},
	})
	discoveryClient.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("partial discovery failure")
	})

	validator := rbac.NewScopeValidator(discoveryClient)
	err := validator.Validate(context.Background(), []collection.AtomicCapability{
		atomicCapability(v1alpha1.ScopeCluster, "", "nodes"),
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "discovering API resources")

	var pending *rbac.APITypePendingError
	assert.False(t, errors.As(err, &pending))
	var invalid *rbac.InvalidCapabilitiesError
	assert.False(t, errors.As(err, &invalid))
}

func TestScopeValidatorRejectsConflictingDiscoveryScopes(t *testing.T) {
	validator := rbac.NewScopeValidator(testDiscovery(
		&metav1.APIResourceList{
			GroupVersion: "example.io/v1",
			APIResources: []metav1.APIResource{{Name: "widgets", Namespaced: true}},
		},
		&metav1.APIResourceList{
			GroupVersion: "example.io/v2",
			APIResources: []metav1.APIResource{{Name: "widgets", Namespaced: false}},
		},
	))

	err := validator.Validate(context.Background(), []collection.AtomicCapability{
		atomicCapability(v1alpha1.ScopeAllNamespaces, "example.io", "widgets"),
	})
	assert.ErrorContains(t, err, "conflicting scopes")
}
