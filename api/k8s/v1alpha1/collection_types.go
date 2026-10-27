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
	Values map[string]string `json:"values,omitempty"`

	// StatusPatches are evaluated post-apply to set status on rendered objects.
	// Each entry targets one object by kind+name and provides a CEL expression
	// or a static map (with CEL in values) that produces the status to write.
	StatusPatches []StatusPatch `json:"statusPatches,omitempty"`
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
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type ResourceCollectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceCollection `json:"items"`
}
