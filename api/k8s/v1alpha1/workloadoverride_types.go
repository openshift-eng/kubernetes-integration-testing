package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type WorkloadOverride struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkloadOverrideSpec   `json:"spec"`
	Status            WorkloadOverrideStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!(self.noOp && has(self.containers) && size(self.containers) > 0)",message="noOp is mutually exclusive with containers"
// +kubebuilder:validation:XValidation:rule="self.noOp || (has(self.containers) && size(self.containers) > 0)",message="noOp or at least one container override must be set"
type WorkloadOverrideSpec struct {
	// Match selects the Pods affected by this override. The runtime selector is
	// evaluated against the admitted Pod object, not against its owning
	// Deployment, DaemonSet, Job, or Pod template.
	//
	// All non-empty match fields must match the same Pod. A field that is absent
	// does not constrain the match, but at least one field must be non-empty.
	// +kubebuilder:validation:Required
	Match WorkloadOverrideMatch `json:"match"`

	// NoOp suppresses execution of every container in matching pods.
	NoOp bool `json:"noOp,omitempty"`

	// Containers declares per-container field overrides.
	// Each entry targets one existing container by name.
	// +listType=map
	// +listMapKey=name
	Containers []ContainerOverride `json:"containers,omitempty"`
}

// WorkloadOverrideMatch is a conjunction of exact Pod match fields.
//
// It is intentionally simpler than a Kubernetes LabelSelector: labels support
// only exact key and value equality, with no set-based or expression-based
// matching.
//
// Because WorkloadOverride is cluster-scoped, namespace is the namespace of the
// target Pods rather than the namespace containing this resource.
// +kubebuilder:validation:XValidation:rule="(has(self.namespace) && size(self.namespace) > 0) || (has(self.labels) && size(self.labels) > 0) || (has(self.podName) && size(self.podName) > 0) || (has(self.nodeName) && size(self.nodeName) > 0)",message="at least one non-empty match field must be set"
type WorkloadOverrideMatch struct {
	// Namespace restricts the match to Pods in this namespace. An empty value
	// does not constrain the namespace.
	Namespace string `json:"namespace,omitempty"`

	// Labels requires every key and value to be present on the Pod with the
	// same value. An empty map does not constrain labels and is not by itself
	// a valid match.
	Labels map[string]string `json:"labels,omitempty"`

	// PodName restricts the match to one exact Pod name. Generated Pod names
	// change on recreation, so labels are usually more stable for controller-
	// owned workloads.
	PodName string `json:"podName,omitempty"`

	// NodeName restricts the match to Pods assigned to this exact node. The
	// admission webhook observes Pods on CREATE, before the scheduler has
	// assigned a node to an ordinary Pod. This field is therefore most useful
	// for Pods created with an explicit node assignment, such as DaemonSet
	// Pods.
	NodeName string `json:"nodeName,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!self.remove || (!has(self.image) && !has(self.pullPolicy) && !has(self.command) && !has(self.args) && !has(self.env))",message="remove is mutually exclusive with other container fields"
// +kubebuilder:validation:XValidation:rule="self.remove || has(self.image) || has(self.pullPolicy) || has(self.command) || has(self.args) || (has(self.env) && size(self.env) > 0)",message="at least one override field must be set"
type ContainerOverride struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Remove removes the named container from the pod.
	// Mutually exclusive with all other container fields.
	Remove bool `json:"remove,omitempty"`

	// +kubebuilder:validation:MinLength=1
	Image string `json:"image,omitempty"`
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	// +listType=map
	// +listMapKey=name
	Env []EnvOverride `json:"env,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.value) != self.remove",message="exactly one of value or remove must be set"
type EnvOverride struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z][-._a-zA-Z0-9]*$`
	Name string `json:"name"`

	Value  string `json:"value,omitempty"`
	Remove bool   `json:"remove,omitempty"`
}

const ConditionOverrideApplied = "Applied"

type WorkloadOverrideStatus struct {
	// MatchedPods is the number of live Pods matching spec.match at the last
	// observation. It is a point-in-time observation, not the expected replica
	// count or the number of Pods that have ever matched.
	// +kubebuilder:validation:Minimum=0
	MatchedPods int32 `json:"matchedPods"`

	// AppliedPods is the number of matched live Pods whose effective admitted
	// specification contains this override. It is a point-in-time observation.
	// +kubebuilder:validation:Minimum=0
	AppliedPods int32 `json:"appliedPods"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type WorkloadOverrideList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadOverride `json:"items"`
}
