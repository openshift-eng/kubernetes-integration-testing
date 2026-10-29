package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type CertificateAuthority struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CertificateAuthoritySpec   `json:"spec"`
	Status            CertificateAuthorityStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.commonName == oldSelf.commonName",message="commonName is immutable"
type CertificateAuthoritySpec struct {
	// +kubebuilder:validation:Required
	CommonName string          `json:"commonName"`
	Validity   metav1.Duration `json:"validity,omitempty"`
}

const ConditionCAReady = "Ready"

type SignedCertificateRef struct {
	corev1.SecretReference `json:",inline"`
	CommonName             string `json:"commonName"`
}

type CertificateAuthorityStatus struct {
	SecretRef          *corev1.SecretReference `json:"secretRef,omitempty"`
	NotAfter           metav1.Time             `json:"notAfter,omitempty"`
	SignedCertificates []SignedCertificateRef   `json:"signedCertificates,omitempty"`
	Conditions         []metav1.Condition      `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type CertificateAuthorityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificateAuthority `json:"items"`
}
