package kube_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/kube"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestPolicy(t *testing.T) {
	p := kube.NewPolicy()

	cases := []struct {
		name    string
		gvr     schema.GroupVersionResource
		verb    string
		wantErr bool
	}{
		{
			name:    "pods get allowed",
			gvr:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			verb:    "get",
			wantErr: false,
		},
		{
			name:    "pods create allowed",
			gvr:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			verb:    "create",
			wantErr: false,
		},
		{
			name:    "secrets get denied",
			gvr:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
			verb:    "get",
			wantErr: true,
		},
		{
			name:    "secrets list denied",
			gvr:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
			verb:    "list",
			wantErr: true,
		},
		{
			name:    "secrets create denied",
			gvr:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
			verb:    "create",
			wantErr: true,
		},
		{
			name:    "kit resources get allowed",
			gvr:     schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "kitclusters"},
			verb:    "get",
			wantErr: false,
		},
		{
			name:    "kit resources list allowed",
			gvr:     schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "kitclusters"},
			verb:    "list",
			wantErr: false,
		},
		{
			name:    "kit resources watch allowed",
			gvr:     schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "kitclusters"},
			verb:    "watch",
			wantErr: false,
		},
		{
			name:    "kit resources create denied",
			gvr:     schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "kitclusters"},
			verb:    "create",
			wantErr: true,
		},
		{
			name:    "kit resources update denied",
			gvr:     schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "agents"},
			verb:    "update",
			wantErr: true,
		},
		{
			name:    "kit resources patch denied",
			gvr:     schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "workloadoverrides"},
			verb:    "patch",
			wantErr: true,
		},
		{
			name:    "kit resources delete denied",
			gvr:     schema.GroupVersionResource{Group: "kit.openshift.io", Version: "v1alpha1", Resource: "resourcecollections"},
			verb:    "delete",
			wantErr: true,
		},
		{
			name:    "apps deployments create allowed",
			gvr:     schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			verb:    "create",
			wantErr: false,
		},
		{
			name:    "nodes get allowed",
			gvr:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"},
			verb:    "get",
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.CheckAccess(tc.gvr, tc.verb)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
