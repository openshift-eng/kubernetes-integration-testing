package v1alpha1

import (
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceCollection declares a set of bootstrap resources to render and apply
// to the cluster. Templates are resolved from a named source (builtin
// or filesystem) or provided inline.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type ResourceCollection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ResourceCollectionSpec   `json:"spec"`
	Status            ResourceCollectionStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!(has(self.source) && has(self.inline))",message="source and inline are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.source) || has(self.inline) || (has(self.scripts) && size(self.scripts) > 0) || (has(self.scriptCollections) && size(self.scriptCollections) > 0)",message="one of source, inline, scripts, or scriptCollections must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.scripts) || !self.scripts.exists(s, has(s.root) && size(s.root) > 0) || has(self.source)",message="scripts with root require source"
type ResourceCollectionSpec struct {
	// Source is the name of a builtin or filesystem-provided collection.
	// Mutually exclusive with Inline.
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source,omitempty"`

	// Inline provides templates directly in the CR.
	// Mutually exclusive with Source.
	// +kubebuilder:validation:MinItems=1
	Inline []InlineTemplate `json:"inline,omitempty"`

	// Values are user-defined variables accessible as {{ .Values.key }} in templates.
	// Each value can be any JSON primitive (string, number, bool, array, object).
	Values map[string]apiextensionsv1.JSON `json:"values,omitempty"`

	// Scripts declares script packages available for Agent CRs.
	// Each package is loaded from a directory or inline content. Packages with
	// an entrypoint are runnable (Agent targets them); packages without are
	// libraries available to runnable packages via require("<package>/<file>").
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Scripts []ScriptPackage `json:"scripts,omitempty"`

	// ScriptCollections lists other ResourceCollections whose script
	// packages are merged into this collection's bundle. Dependencies
	// are resolved transitively. An Agent's entrypoint must target a
	// local package, not one from a dependency.
	// +kubebuilder:validation:MaxItems=32
	ScriptCollections []string `json:"scriptCollections,omitempty"`

	// StatusPatches are evaluated post-apply to set status on rendered objects.
	// Each entry targets one object by kind+name and provides a CEL expression
	// or a static map (with CEL in values) that produces the status to write.
	StatusPatches []StatusPatch `json:"statusPatches,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.root) && size(self.root) > 0 ? 1 : 0) + (has(self.inline) && size(self.inline) > 0 ? 1 : 0) == 1",message="exactly one of root or inline must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.include) && size(self.include) > 0) || (has(self.root) && size(self.root) > 0)",message="include is only valid with root"
// +kubebuilder:validation:XValidation:rule="!(has(self.entrypoint) && size(self.entrypoint) > 0) || (has(self.root) && size(self.root) > 0)",message="entrypoint is only valid with root"
type ScriptPackage struct {
	// Name identifies this script package within the collection.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Label().validate(self).hasValue()",message="name must be a valid DNS label"
	Name string `json:"name"`

	// Root is a directory path relative to the collection root.
	// All files under root are included unless filtered by include globs.
	// +kubebuilder:validation:MaxLength=256
	Root string `json:"root,omitempty"`

	// Include restricts which files under root are part of the package.
	// Each entry is a glob pattern matched against the relative path.
	// Supports ** for recursive matching. Only valid when root is set.
	// +kubebuilder:validation:MaxItems=32
	Include []string `json:"include,omitempty"`

	// Inline provides the script content directly in the CR.
	// Inline packages are always runnable with the inline content as the
	// entrypoint.
	// +kubebuilder:validation:MaxLength=1048576
	Inline string `json:"inline,omitempty"`

	// Entrypoint is the path to the main script file, relative to the
	// package root. Only valid with root. Packages with an entrypoint are
	// runnable (Agent CRs can reference them); packages without are
	// libraries available via require("<package-name>/<file>").
	// +kubebuilder:validation:MaxLength=256
	Entrypoint string `json:"entrypoint,omitempty"`

	// Capabilities declares the Kubernetes API access this package needs.
	// Kit compiles them into generated RBAC and enforces them at runtime.
	// +kubebuilder:validation:MaxItems=32
	Capabilities []ScriptCapability `json:"capabilities,omitempty"`
}

// ScriptCapabilityScope determines where a capability applies and what
// RBAC objects Kit generates for it.
// +kubebuilder:validation:Enum=targetNamespace;allNamespaces;cluster
type ScriptCapabilityScope string

const (
	// ScopeTargetNamespace restricts access to the Agent's target namespace.
	ScopeTargetNamespace ScriptCapabilityScope = "targetNamespace"
	// ScopeAllNamespaces grants access to a namespaced resource across all namespaces.
	ScopeAllNamespaces ScriptCapabilityScope = "allNamespaces"
	// ScopeCluster grants access to a cluster-scoped resource.
	ScopeCluster ScriptCapabilityScope = "cluster"
)

// ScriptCapability declares access to a set of Kubernetes resources.
// It embeds rbacv1.PolicyRule so the compiler can consume it directly.
// Fields not supported in v1 (resourceNames, nonResourceURLs) are
// rejected by CRD validation. Semantic checks (wildcard rejection,
// verb allowlist, empty resource) are deferred to the loader.
// +kubebuilder:validation:XValidation:rule="size(self.apiGroups) >= 1",message="at least one apiGroup is required"
// +kubebuilder:validation:XValidation:rule="size(self.resources) >= 1",message="at least one resource is required"
// +kubebuilder:validation:XValidation:rule="size(self.verbs) >= 1 && size(self.verbs) <= 7",message="verbs must have between 1 and 7 entries"
// +kubebuilder:validation:XValidation:rule="!has(self.resourceNames) || size(self.resourceNames) == 0",message="resourceNames is not supported"
// +kubebuilder:validation:XValidation:rule="!has(self.nonResourceURLs) || size(self.nonResourceURLs) == 0",message="nonResourceURLs is not supported"
type ScriptCapability struct {
	// Scope determines where this capability applies.
	// +kubebuilder:validation:Required
	Scope ScriptCapabilityScope `json:"scope"`

	rbacv1.PolicyRule `json:",inline"`
}

// StatusPatch declares a status to apply to a rendered object after it has been
// created. The Status field can be either a CEL expression string that evaluates
// to a map, or a YAML map whose leaf values are CEL expressions.
type StatusPatch struct {
	// Kind of the target object.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Name of the target object.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Status is the status to set. It can be a CEL expression (string) that
	// returns a map, or a structured map with CEL expressions in leaf values.
	// +kubebuilder:validation:Required
	Status apiextensionsv1.JSON `json:"status"`
}

type InlineTemplate struct {
	// +kubebuilder:validation:Required
	Content string `json:"content"`
}

const (
	// ConditionCollectionReady reports whether the collection is healthy:
	// its content renders and hashes. Whether the content is live on the
	// cluster is reported by ConditionCollectionApplied.
	ConditionCollectionReady = "Ready"
	// ConditionCollectionApplied reports whether the collection's
	// declared content (templates and status patches) is live on the
	// cluster.
	ConditionCollectionApplied = "Applied"
	// ConditionCollectionContentReady reports whether the collection's
	// content renders and hashes successfully.
	ConditionCollectionContentReady = "ContentReady"

	// ReasonCollectionAppliedNotRequested marks Applied=Unknown on a
	// collection with apply work that no plan step has requested yet.
	ReasonCollectionAppliedNotRequested = "AppliedNotRequested"
	// ReasonCollectionAppliedPlanPending marks Applied=Unknown on a
	// collection referenced only by plan steps that have not been
	// reached yet.
	ReasonCollectionAppliedPlanPending = "AppliedPlanPending"
	// ReasonCollectionAppliedOutdated marks Applied=Unknown on a
	// collection whose applied content is still live but no longer
	// matches its current content.
	ReasonCollectionAppliedOutdated = "AppliedOutdated"
	// ReasonCollectionNotReferenced marks Ready=Unknown on a collection
	// that nothing references.
	ReasonCollectionNotReferenced = "NotReferenced"
)

type ResourceCollectionStatus struct {
	// ContentHash is a deterministic hash of the collection's declared
	// content: rendered manifests, scripts, and status patches.
	ContentHash string `json:"contentHash,omitempty"`

	// AppliedHash is the ContentHash of the content that is live on the
	// cluster; empty means the collection was never applied. It differs
	// from ContentHash when the content changed after the last apply.
	AppliedHash string `json:"appliedHash,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type ResourceCollectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceCollection `json:"items"`
}
