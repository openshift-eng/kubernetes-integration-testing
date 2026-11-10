package kube

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Policy enforces host-level access control for agent scripts.
// Agents operate with the pod's ServiceAccount but Policy adds
// an additional layer that denies dangerous operations regardless
// of what RBAC would allow.
type Policy struct{}

func NewPolicy() *Policy { return &Policy{} }

// CheckAccess returns an error if the operation is denied by policy.
func (p *Policy) CheckAccess(gvr schema.GroupVersionResource, verb string) error {
	if gvr.Group == "" && gvr.Resource == "secrets" {
		return fmt.Errorf("access to secrets is denied by agent policy")
	}
	if gvr.Group == "kit.openshift.io" && isMutatingVerb(verb) {
		return fmt.Errorf("%s on kit.openshift.io/%s is denied by agent policy: Kit resources are read-only", verb, gvr.Resource)
	}
	return nil
}

func isMutatingVerb(verb string) bool {
	switch verb {
	case "create", "update", "patch", "delete":
		return true
	}
	return false
}
