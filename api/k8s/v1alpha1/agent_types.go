package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=js
// +enum
type AgentRuntime string

const (
	AgentRuntimeJS AgentRuntime = "js"
)

// +kubebuilder:validation:Enum=pod;container
// +enum
type AgentReplaceMode string

const (
	AgentReplacePod       AgentReplaceMode = "pod"
	AgentReplaceContainer AgentReplaceMode = "container"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentSpec   `json:"spec"`
	Status            AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.timeout) || duration(self.timeout) > duration('0s')",message="timeout must be positive"
type AgentSpec struct {
	// Target selects the pods and container to replace with the agent.
	// +kubebuilder:validation:Required
	Target AgentTarget `json:"target"`

	// Runtime selects the script engine.
	// +kubebuilder:validation:Required
	Runtime AgentRuntime `json:"runtime"`

	// Script references a package in a ResourceCollection.
	// +kubebuilder:validation:Required
	Script AgentScriptRef `json:"script"`

	// Values are user-defined variables passed to the script as ctx.values.
	Values map[string]apiextensionsv1.JSON `json:"values,omitempty"`

	// Timeout limits the total execution time of the agent script.
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.replace != 'container' || (has(self.container) && size(self.container) > 0)",message="target.container is required when replace is container"
// +kubebuilder:validation:XValidation:rule="self.replace != 'pod' || !has(self.container) || size(self.container) == 0",message="target.container must not be set when replace is pod"
// +kubebuilder:validation:XValidation:rule="self.replace != 'pod' || (has(self.labels) && size(self.labels) > 0) || (has(self.podName) && size(self.podName) > 0) || (has(self.nodeName) && size(self.nodeName) > 0)",message="replace pod requires at least one selector beyond namespace (labels, podName, or nodeName)"
type AgentTarget struct {
	// Namespace is the target namespace where the agent identity and generated
	// WorkloadOverride will take effect. Does not need to exist when the Agent
	// is created.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Label().validate(self).hasValue()",message="namespace must be a valid DNS label"
	Namespace string `json:"namespace"`

	// Replace selects the replacement boundary. "pod" replaces the entire Pod
	// workload with one agent container, removing all original containers.
	// "container" replaces exactly one named container and preserves the rest.
	// +kubebuilder:default=pod
	Replace AgentReplaceMode `json:"replace,omitempty"`

	// Labels restricts the match to Pods carrying every listed label with the
	// exact value. Same semantics as WorkloadOverride.
	Labels map[string]string `json:"labels,omitempty"`

	// PodName restricts the match to one exact Pod name.
	PodName string `json:"podName,omitempty"`

	// NodeName restricts the match to Pods assigned to this node.
	NodeName string `json:"nodeName,omitempty"`

	// Container is the regular container name to replace with the agent binary.
	// Required when replace is "container", must not be set when replace is "pod".
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Label().validate(self).hasValue()",message="container must be a valid DNS label"
	Container string `json:"container,omitempty"`
}

type AgentScriptRef struct {
	// Collection is the name of the cluster-scoped ResourceCollection.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Subdomain().validate(self).hasValue()",message="collection must be a valid DNS subdomain"
	Collection string `json:"collection"`

	// Package is the name of the script package within the collection.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!format.dns1123Label().validate(self).hasValue()",message="package must be a valid DNS label"
	Package string `json:"package"`
}

const (
	ConditionAgentReady = "Ready"

	ReasonAgentReady            = "Ready"
	ReasonAgentNamespacePending = "NamespacePending"
	ReasonAgentSuperseded       = "Superseded"
	ReasonAgentCollectionError  = "CollectionError"
	ReasonAgentBundleError      = "BundleError"
)

type AgentStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BundleDigest is the SHA256 hex digest of the current resolved bundle.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	BundleDigest string `json:"bundleDigest,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}
