package kube

import (
	"fmt"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// AccessRequest describes a single Kubernetes API operation for
// authorization. GVR and Verb identify what is being accessed; Namespace
// and Subresource capture where and at what sub-level.
type AccessRequest struct {
	GVR         schema.GroupVersionResource
	Verb        string
	Namespace   string
	Subresource string
}

// Policy is a capability-based allowlist. Every operation must have an
// exact capability match (scope + group + resource/subresource + verb +
// namespace) or it is denied.
type Policy struct {
	targetNamespace string
	entries         []policyEntry
}

type policyEntry struct {
	scope    string
	apiGroup string
	resource string // may include subresource slash, e.g. "machineconfignodes/status"
	verb     string
}

// NewPolicy builds a fail-closed allowlist from the bundle-declared
// capabilities. An empty/nil capability slice means no API access is
// allowed.
func NewPolicy(targetNamespace string, caps []collection.AtomicCapability) *Policy {
	entries := make([]policyEntry, 0, len(caps))
	for _, c := range caps {
		entries = append(entries, policyEntry{
			scope:    string(c.Scope),
			apiGroup: c.APIGroup,
			resource: c.Resource,
			verb:     c.Verb,
		})
	}
	return &Policy{targetNamespace: targetNamespace, entries: entries}
}

// CheckAccess returns nil when req is allowed by at least one capability,
// otherwise an actionable error.
func (p *Policy) CheckAccess(req AccessRequest) error {
	// Normalize "apply" to "patch" — the runtime maps SSA to Patch under
	// the hood, so a capability granting patch covers apply.
	verb := req.Verb
	if verb == "apply" {
		verb = "patch"
	}

	// Reconstruct the full resource string (including subresource).
	resource := req.GVR.Resource
	if req.Subresource != "" {
		resource = resource + "/" + req.Subresource
	}

	for _, e := range p.entries {
		if e.apiGroup != req.GVR.Group {
			continue
		}
		if e.resource != resource {
			continue
		}
		if e.verb != verb {
			continue
		}
		if !p.namespaceAllowed(e.scope, req) {
			continue
		}
		return nil
	}

	scopeHint := p.scopeHintFor(req, verb, resource)
	if scopeHint != "" {
		return fmt.Errorf("missing capability: %s %s/%s at %s scope (%s)", verb, req.GVR.Group, resource, scopeHint, namespaceDetail(req))
	}
	return fmt.Errorf("missing capability: %s %s/%s (%s)", verb, req.GVR.Group, resource, namespaceDetail(req))
}

func (p *Policy) namespaceAllowed(scope string, req AccessRequest) bool {
	switch scope {
	case "targetNamespace":
		return req.Namespace == p.targetNamespace
	case "allNamespaces":
		if req.Namespace != "" {
			return true
		}
		// Empty namespace on a namespaced resource means
		// "all namespaces" — only list/watch make sense there.
		return req.Verb == "list" || req.Verb == "watch"
	case "cluster":
		return req.Namespace == ""
	default:
		return false
	}
}

func (p *Policy) scopeHintFor(req AccessRequest, verb, resource string) string {
	for _, e := range p.entries {
		if e.apiGroup == req.GVR.Group && e.resource == resource && e.verb == verb {
			return e.scope
		}
	}
	return ""
}

func namespaceDetail(req AccessRequest) string {
	if req.Namespace == "" {
		return "cluster scope"
	}
	return fmt.Sprintf("namespace %q", req.Namespace)
}
