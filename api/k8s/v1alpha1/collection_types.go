package v1alpha1

import (
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

// +kubebuilder:validation:XValidation:rule="has(self.source) != has(self.inline)",message="exactly one of source or inline must be set"
type ResourceCollectionSpec struct {
	// Source is the name of a builtin or filesystem-provided collection.
	// Mutually exclusive with Inline.
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
	Scripts []ScriptPackage `json:"scripts,omitempty"`

	// ScriptCollections lists the names of other ResourceCollections whose
	// script packages are included in the bundle alongside local packages.
	// At runtime, all packages (local and from referenced collections) are
	// available via require("<package-name>/<file>"). Collection-level cycles
	// are rejected to prevent finalizer deadlocks.
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
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Label().validate(self).hasValue()",message="name must be a valid DNS label"
	Name string `json:"name"`

	// Root is a directory path relative to the collection root.
	// All files under root are included unless filtered by include globs.
	Root string `json:"root,omitempty"`

	// Include restricts which files under root are part of the package.
	// Each entry is a glob pattern matched against the relative path.
	// Supports ** for recursive matching. Only valid when root is set.
	// +kubebuilder:validation:MaxItems=32
	Include []string `json:"include,omitempty"`

	// Inline provides the script content directly in the CR.
	// Inline packages are always runnable with the inline content as the
	// entrypoint.
	Inline string `json:"inline,omitempty"`

	// Entrypoint is the path to the main script file, relative to the
	// package root. Only valid with root. Packages with an entrypoint are
	// runnable (Agent CRs can reference them); packages without are
	// libraries available via require("<package-name>/<file>").
	Entrypoint string `json:"entrypoint,omitempty"`
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
	ConditionCollectionApplied = "Applied"
)

type ResourceCollectionStatus struct {
	ContentHash string             `json:"contentHash,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type ResourceCollectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceCollection `json:"items"`
}
