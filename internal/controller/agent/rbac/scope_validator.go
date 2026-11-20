package rbac

import (
	"context"
	"fmt"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// ScopeValidator verifies that declared capability scopes match API discovery.
type ScopeValidator struct {
	discovery discovery.DiscoveryInterfaceWithContext
}

// NewScopeValidator creates a validator backed by the given discovery client.
func NewScopeValidator(discoveryClient discovery.DiscoveryInterface) *ScopeValidator {
	return &ScopeValidator{discovery: discovery.ToDiscoveryInterfaceWithContext(discoveryClient)}
}

// APITypePendingError indicates that a declared resource is not served yet.
type APITypePendingError struct {
	APIGroup string
	Resource string
}

func (e *APITypePendingError) Error() string {
	return fmt.Sprintf("API type %s/%s is not served yet", displayGroup(e.APIGroup), e.Resource)
}

// InvalidCapabilitiesError indicates that a capability scope conflicts with
// the discovered resource scope.
type InvalidCapabilitiesError struct {
	Capability collection.AtomicCapability
	Namespaced bool
}

func (e *InvalidCapabilitiesError) Error() string {
	actualScope := "cluster-scoped"
	if e.Namespaced {
		actualScope = "namespaced"
	}
	return fmt.Sprintf(
		"capability scope %q is invalid for %s/%s: resource is %s",
		e.Capability.Scope,
		displayGroup(e.Capability.APIGroup),
		e.Capability.Resource,
		actualScope,
	)
}

type resourceKey struct {
	apiGroup string
	resource string
}

// Validate checks every capability against the currently served API resources.
func (v *ScopeValidator) Validate(ctx context.Context, capabilities []collection.AtomicCapability) error {
	if len(capabilities) == 0 {
		return nil
	}

	_, resourceLists, err := v.discovery.ServerGroupsAndResourcesWithContext(ctx)
	if err != nil {
		return fmt.Errorf("discovering API resources: %w", err)
	}

	scopes := make(map[resourceKey]bool)
	for _, list := range resourceLists {
		if list == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return fmt.Errorf("parsing discovered group version %q: %w", list.GroupVersion, err)
		}
		for _, resource := range list.APIResources {
			key := resourceKey{apiGroup: gv.Group, resource: resource.Name}
			if namespaced, exists := scopes[key]; exists && namespaced != resource.Namespaced {
				return fmt.Errorf(
					"discovery returned conflicting scopes for %s/%s",
					displayGroup(key.apiGroup),
					key.resource,
				)
			}
			scopes[key] = resource.Namespaced
		}
	}

	for _, capability := range capabilities {
		key := resourceKey{
			apiGroup: capability.APIGroup,
			resource: capability.Resource,
		}
		namespaced, found := scopes[key]
		if !found {
			return &APITypePendingError{
				APIGroup: capability.APIGroup,
				Resource: capability.Resource,
			}
		}

		valid := false
		switch capability.Scope {
		case v1alpha1.ScopeTargetNamespace, v1alpha1.ScopeAllNamespaces:
			valid = namespaced
		case v1alpha1.ScopeCluster:
			valid = !namespaced
		}
		if !valid {
			return &InvalidCapabilitiesError{
				Capability: capability,
				Namespaced: namespaced,
			}
		}
	}

	return nil
}

func displayGroup(group string) string {
	if group == "" {
		return "core"
	}
	return group
}
