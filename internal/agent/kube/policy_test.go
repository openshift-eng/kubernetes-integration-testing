package kube_test

import (
	"testing"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func caps(caps ...collection.AtomicCapability) []collection.AtomicCapability { return caps }

func capAt(scope v1alpha1.ScriptCapabilityScope, group, resource, verb string) collection.AtomicCapability {
	return collection.AtomicCapability{Scope: scope, APIGroup: group, Resource: resource, Verb: verb}
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		name            string
		targetNamespace string
		caps            []collection.AtomicCapability
		req             kube.AccessRequest
		wantErr         bool
	}{
		{
			name:            "targetNamespace: get in own namespace allowed",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeTargetNamespace, "", "pods", "get")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "pods"}, Verb: "get", Namespace: "my-ns"},
		},
		{
			name:            "targetNamespace: get in other namespace denied",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeTargetNamespace, "", "pods", "get")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "pods"}, Verb: "get", Namespace: "other-ns"},
			wantErr:         true,
		},
		{
			name:            "targetNamespace: list in other namespace denied",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeTargetNamespace, "", "pods", "list")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "pods"}, Verb: "list", Namespace: "other-ns"},
			wantErr:         true,
		},
		{
			name:            "allNamespaces: get in any namespace allowed",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeAllNamespaces, "", "configmaps", "get")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "configmaps"}, Verb: "get", Namespace: "other-ns"},
		},
		{
			name:            "allNamespaces: list with empty namespace allowed",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeAllNamespaces, "", "configmaps", "list")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "configmaps"}, Verb: "list", Namespace: ""},
		},
		{
			name:            "allNamespaces: watch with empty namespace allowed",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeAllNamespaces, "", "configmaps", "watch")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "configmaps"}, Verb: "watch", Namespace: ""},
		},
		{
			name:            "allNamespaces: get with empty namespace denied",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeAllNamespaces, "", "configmaps", "get")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "configmaps"}, Verb: "get", Namespace: ""},
			wantErr:         true,
		},
		{
			name:            "allNamespaces: create with empty namespace denied",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeAllNamespaces, "", "configmaps", "create")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "configmaps"}, Verb: "create", Namespace: ""},
			wantErr:         true,
		},
		{
			name:            "cluster: get with empty namespace allowed",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeCluster, "", "nodes", "get")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "nodes"}, Verb: "get", Namespace: ""},
		},
		{
			name:            "cluster: get with namespace denied",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeCluster, "", "nodes", "get")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "nodes"}, Verb: "get", Namespace: "my-ns"},
			wantErr:         true,
		},
		{
			name:            "cluster: subresource patch allowed",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeCluster, "machineconfiguration.openshift.io", "machineconfignodes/status", "patch")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "machineconfiguration.openshift.io", Resource: "machineconfignodes"}, Verb: "patch", Namespace: "", Subresource: "status"},
		},
		{
			name:            "cluster: subresource patch without declaration denied",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeCluster, "machineconfiguration.openshift.io", "machineconfignodes", "patch")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "machineconfiguration.openshift.io", Resource: "machineconfignodes"}, Verb: "patch", Namespace: "", Subresource: "status"},
			wantErr:         true,
		},
		{
			name:            "apply is authorized as patch",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeTargetNamespace, "", "configmaps", "patch")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "configmaps"}, Verb: "apply", Namespace: "my-ns"},
		},
		{
			name:            "empty capabilities denies everything",
			targetNamespace: "my-ns",
			caps:            nil,
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "pods"}, Verb: "get", Namespace: "my-ns"},
			wantErr:         true,
		},
		{
			name:            "wrong group denied",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeTargetNamespace, "apps", "deployments", "get")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "deployments"}, Verb: "get", Namespace: "my-ns"},
			wantErr:         true,
		},
		{
			name:            "wrong verb denied",
			targetNamespace: "my-ns",
			caps:            caps(capAt(v1alpha1.ScopeTargetNamespace, "", "pods", "get")),
			req:             kube.AccessRequest{GVR: schema.GroupVersionResource{Group: "", Resource: "pods"}, Verb: "list", Namespace: "my-ns"},
			wantErr:         true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := kube.NewPolicy(tc.targetNamespace, tc.caps)
			err := p.CheckAccess(tc.req)
			if tc.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "missing capability")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
