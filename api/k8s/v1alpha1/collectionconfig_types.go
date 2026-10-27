package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CollectionConfig is a cluster-scoped singleton that tracks available
// ResourceCollections. Future versions may add spec fields for merge
// strategy, ordering, or conflict resolution.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type CollectionConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Status            CollectionConfigStatus `json:"status,omitempty"`
}

type CollectionConfigStatus struct {
	Available []string `json:"available,omitempty"`
}

// +kubebuilder:object:root=true
type CollectionConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CollectionConfig `json:"items"`
}
