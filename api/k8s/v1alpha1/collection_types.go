package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
