package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// DeploymentPlan declares an ordered sequence of resources to deploy.
// Steps are applied in array order; each step is either a collection
// of templates or a set of manifests filtered from the release image.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type DeploymentPlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DeploymentPlanSpec   `json:"spec"`
	Status            DeploymentPlanStatus `json:"status,omitempty"`
}

type DeploymentPlanSpec struct {
	// +kubebuilder:validation:MinItems=1
	Steps []DeploymentStep `json:"steps"`
}

// +kubebuilder:validation:XValidation:rule="has(self.collection) != has(self.release)",message="exactly one of collection or release must be set"
type DeploymentStep struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	Collection *CollectionStep `json:"collection,omitempty"`
	Release    *ReleaseStep    `json:"release,omitempty"`
}

type CollectionStep struct {
	// +kubebuilder:validation:Required
	Source string `json:"source"`
}

type ReleaseStep struct {
	// +kubebuilder:validation:Required
	FilenameRegex string `json:"filenameRegex"`
}

type DeploymentStepPhase string

const (
	StepPending   DeploymentStepPhase = "Pending"
	StepRequested DeploymentStepPhase = "Requested"
	StepReady     DeploymentStepPhase = "Ready"
	StepFailed    DeploymentStepPhase = "Failed"
)

const (
	ConditionPlanReady = "Ready"
)

type DeploymentPlanStatus struct {
	Steps      []DeploymentStepStatus `json:"steps,omitempty"`
	Conditions []metav1.Condition     `json:"conditions,omitempty"`
}

type DeploymentStepStatus struct {
	Name        string              `json:"name"`
	Phase       DeploymentStepPhase `json:"phase"`
	ContentHash string              `json:"contentHash,omitempty"`
	Message     string              `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type DeploymentPlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DeploymentPlan `json:"items"`
}
