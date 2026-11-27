package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// KitCluster is the singleton cluster configuration.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="metadata.name must be 'cluster'"
type KitCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KitClusterSpec   `json:"spec"`
	Status            KitClusterStatus `json:"status,omitempty"`
}

type KitClusterSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=30
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterName is immutable"
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('kit-')",message="clusterName must not start with 'kit-'"
	ClusterName string `json:"clusterName"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=kwok;kind
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider is immutable"
	Provider string `json:"provider"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="image is immutable"
	Image string `json:"image,omitempty"`

	// kubeVersion is the Kubernetes version of the cluster control
	// plane, e.g. v1.36 or v1.36.4; a minor resolves to the latest
	// published patch. When empty, the provider picks its default.
	// +optional
	// +kubebuilder:validation:Pattern=`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="kubeVersion is immutable"
	KubeVersion string `json:"kubeVersion,omitempty"`

	// +kubebuilder:validation:Enum=Default;TechPreviewNoUpgrade
	FeatureSet string `json:"featureSet,omitempty"`

	// +kubebuilder:validation:Minimum=0
	Workers int `json:"workers,omitempty"`

	DefaultCA string `json:"defaultCA,omitempty"`
}

const (
	ConditionReady         = "Ready"
	ConditionImagePulled   = "ImagePulled"
	ConditionCRDsInstalled = "CRDsInstalled"
)

type KitClusterStatus struct {
	APIServerURL         string `json:"apiServerURL,omitempty"`
	APIServerExternalURL string `json:"apiServerExternalURL,omitempty"`

	// kubeVersion is the Kubernetes version the control plane runs, as
	// reported by the API server.
	// +kubebuilder:validation:Pattern=`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`
	KubeVersion string `json:"kubeVersion"`

	DefaultCA  string             `json:"defaultCA,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type KitClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KitCluster `json:"items"`
}
