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

}

const (
	ConditionReady         = "Ready"
	ConditionImagePulled   = "ImagePulled"
	ConditionCRDsInstalled = "CRDsInstalled"
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

// OperatorDeployment declares the deployment of operator manifests
// from the release image configured in the KitCluster singleton.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type OperatorDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OperatorDeploymentSpec   `json:"spec"`
	Status            OperatorDeploymentStatus `json:"status,omitempty"`
}

type OperatorDeploymentSpec struct {
	// +kubebuilder:validation:Required
	Include string `json:"include"`
}

type OperatorDeploymentPhase string

const (
	OperatorDeploymentPending   OperatorDeploymentPhase = "Pending"
	OperatorDeploymentDeploying OperatorDeploymentPhase = "Deploying"
	OperatorDeploymentReady     OperatorDeploymentPhase = "Ready"
	OperatorDeploymentFailed    OperatorDeploymentPhase = "Failed"
)

type OperatorDeploymentStatus struct {
	Phase   OperatorDeploymentPhase `json:"phase,omitempty"`
	Message string                 `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type OperatorDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OperatorDeployment `json:"items"`
}

