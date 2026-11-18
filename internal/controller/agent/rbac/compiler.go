package rbac

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelAgentUID = "kit.openshift.io/agent-uid"
	FieldManager  = "kit-agent-materializer"
)

type CompileInput struct {
	Name            string
	TargetNamespace string
	OwnerRef        metav1.OwnerReference
}

// Compile produces the desired RBAC objects for an Agent. The caller
// is responsible for applying them with server-side apply and pruning
// stale objects that are no longer in the returned set.
func Compile(input CompileInput) []client.Object {
	labels := map[string]string{LabelAgentUID: string(input.OwnerRef.UID)}

	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       "Role",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            input.Name,
			Namespace:       input.TargetNamespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{input.OwnerRef},
		},
		Rules: namespacedPolicyRules(),
	}

	rb := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       "RoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            input.Name,
			Namespace:       input.TargetNamespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{input.OwnerRef},
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      input.Name,
			Namespace: input.TargetNamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     input.Name,
		},
	}

	cr := &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       "ClusterRole",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            input.Name,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{input.OwnerRef},
		},
		Rules: clusterScopedPolicyRules(),
	}

	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            input.Name,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{input.OwnerRef},
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      input.Name,
			Namespace: input.TargetNamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     input.Name,
		},
	}

	return []client.Object{role, rb, cr, crb}
}

func namespacedPolicyRules() []rbacv1.PolicyRule {
	allVerbs := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{
				"pods", "pods/log", "pods/status", "services", "configmaps",
				"endpoints", "events", "persistentvolumeclaims",
			},
			Verbs: allVerbs,
		},
		{APIGroups: []string{"apps"}, Resources: []string{"*"}, Verbs: allVerbs},
		{APIGroups: []string{"batch"}, Resources: []string{"*"}, Verbs: allVerbs},
	}
}

// TODO: the OpenShift API groups below are hardcoded for the MCO smoke test.
// This must be replaced by a per-Agent capability set so collections declare
// the access they need instead of granting a fixed superset.
func clusterScopedPolicyRules() []rbacv1.PolicyRule {
	readOnly := []string{"get", "list", "watch"}
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"nodes/status", "persistentvolumes", "namespaces"}, Verbs: readOnly},
		{APIGroups: []string{"kit.openshift.io"}, Resources: []string{"*"}, Verbs: readOnly},
		{APIGroups: []string{"machineconfiguration.openshift.io"}, Resources: []string{"*"}, Verbs: readOnly},
		{APIGroups: []string{"machineconfiguration.openshift.io"}, Resources: []string{"machineconfignodes", "machineconfignodes/status"}, Verbs: []string{"patch"}},
		{APIGroups: []string{"config.openshift.io"}, Resources: []string{"*"}, Verbs: readOnly},
	}
}
