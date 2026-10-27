package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// KitCluster is a cluster-scoped singleton that holds global configuration
// for the testing cluster.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type KitCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KitClusterSpec   `json:"spec"`
	Status            KitClusterStatus `json:"status,omitempty"`
}

type KitClusterSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=kwok;kind
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider is immutable"
	Provider string `json:"provider"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="image is immutable"
	Image string `json:"image,omitempty"`

	// +kubebuilder:validation:Enum=Default;TechPreviewNoUpgrade
	FeatureSet string `json:"featureSet,omitempty"`

	// +kubebuilder:validation:Minimum=0
	Workers int `json:"workers,omitempty"`

	// +kubebuilder:validation:MinItems=1
	Collections []string `json:"collections,omitempty"`
}

const (
	ConditionReady              = "Ready"
	ConditionImagePulled        = "ImagePulled"
	ConditionCRDsInstalled      = "CRDsInstalled"
	ConditionCollectionsApplied = "CollectionsApplied"
)

type KitClusterStatus struct {
	APIServerURL string             `json:"apiServerURL,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type KitClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KitCluster `json:"items"`
}
