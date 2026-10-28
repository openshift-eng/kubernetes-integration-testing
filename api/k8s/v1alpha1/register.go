package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "kit.openshift.io", Version: "v1alpha1"}

	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&KitCluster{},
		&KitClusterList{},
		&ResourceCollection{},
		&ResourceCollectionList{},
		&DeploymentPlan{},
		&DeploymentPlanList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
