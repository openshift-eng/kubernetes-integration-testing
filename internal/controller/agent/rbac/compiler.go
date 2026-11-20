package rbac

import (
	"sort"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelAgentUID = "kit.openshift.io/agent-uid"
	FieldManager  = "kit-agent-materializer"
)

type CompileInput struct {
	Name                  string
	TargetNamespace       string
	OwnerRef              metav1.OwnerReference
	EffectiveCapabilities []collection.AtomicCapability
}

// Compile produces the desired RBAC objects for an Agent from its
// effective capability set. Capabilities with scope targetNamespace
// become a Role + RoleBinding in the target namespace. Capabilities
// with scope allNamespaces or cluster become a ClusterRole +
// ClusterRoleBinding. When no capabilities match a scope, the
// corresponding objects are omitted. With an empty capability set
// no RBAC objects are returned — only the ServiceAccount (managed
// by the caller) is kept.
func Compile(input CompileInput) []client.Object {
	labels := map[string]string{LabelAgentUID: string(input.OwnerRef.UID)}

	nsRules := groupRules(input.EffectiveCapabilities, func(scope v1alpha1.ScriptCapabilityScope) bool {
		return scope == v1alpha1.ScopeTargetNamespace
	})
	clusterRules := groupRules(input.EffectiveCapabilities, func(scope v1alpha1.ScriptCapabilityScope) bool {
		return scope == v1alpha1.ScopeAllNamespaces || scope == v1alpha1.ScopeCluster
	})

	var objects []client.Object

	if len(nsRules) > 0 {
		objects = append(objects,
			&rbacv1.Role{
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
				Rules: nsRules,
			},
			&rbacv1.RoleBinding{
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
			},
		)
	}

	if len(clusterRules) > 0 {
		objects = append(objects,
			&rbacv1.ClusterRole{
				TypeMeta: metav1.TypeMeta{
					APIVersion: rbacv1.SchemeGroupVersion.String(),
					Kind:       "ClusterRole",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:            input.Name,
					Labels:          labels,
					OwnerReferences: []metav1.OwnerReference{input.OwnerRef},
				},
				Rules: clusterRules,
			},
			&rbacv1.ClusterRoleBinding{
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
			},
		)
	}

	return objects
}

type ruleKey struct {
	apiGroup string
	resource string
}

// groupRules selects capabilities matching the predicate, groups them
// by (apiGroup, resource), unions verbs, and returns deterministically
// sorted PolicyRules.
func groupRules(caps []collection.AtomicCapability, match func(v1alpha1.ScriptCapabilityScope) bool) []rbacv1.PolicyRule {
	verbSets := make(map[ruleKey]map[string]struct{})
	for _, cap := range caps {
		if !match(cap.Scope) {
			continue
		}
		k := ruleKey{apiGroup: cap.APIGroup, resource: cap.Resource}
		if verbSets[k] == nil {
			verbSets[k] = make(map[string]struct{})
		}
		verbSets[k][cap.Verb] = struct{}{}
	}

	keys := make([]ruleKey, 0, len(verbSets))
	for k := range verbSets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].apiGroup != keys[j].apiGroup {
			return keys[i].apiGroup < keys[j].apiGroup
		}
		return keys[i].resource < keys[j].resource
	})

	rules := make([]rbacv1.PolicyRule, 0, len(keys))
	for _, k := range keys {
		verbs := make([]string, 0, len(verbSets[k]))
		for v := range verbSets[k] {
			verbs = append(verbs, v)
		}
		sort.Strings(verbs)
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{k.apiGroup},
			Resources: []string{k.resource},
			Verbs:     verbs,
		})
	}
	return rules
}
