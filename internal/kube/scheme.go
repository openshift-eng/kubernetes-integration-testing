package kube

import (
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var Scheme = runtime.NewScheme()

func init() {
	_ = corev1.AddToScheme(Scheme)
	_ = apiextensionsv1.AddToScheme(Scheme)
	_ = v1alpha1.AddToScheme(Scheme)
}
