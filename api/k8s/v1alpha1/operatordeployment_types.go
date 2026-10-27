package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
