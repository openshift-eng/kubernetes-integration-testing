package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Materializer struct {
	client         client.Client
	store          *bundle.BundleStore
	collectionsDir string
	agentImage     string
	bundleBaseURL  string
	log            *slog.Logger
}

type MaterializeResult struct {
	BundleDigest string
}

func NewMaterializer(c client.Client, store *bundle.BundleStore, collectionsDir, agentImage, bundleBaseURL string, log *slog.Logger) *Materializer {
	return &Materializer{
		client:         c,
		store:          store,
		collectionsDir: collectionsDir,
		agentImage:     agentImage,
		bundleBaseURL:  bundleBaseURL,
		log:            log,
	}
}

func (m *Materializer) Materialize(ctx context.Context, ag *v1alpha1.Agent) (*MaterializeResult, error) {
	packages, entrypointPkg, err := m.loadPackages(ctx, ag)
	if err != nil {
		return nil, err
	}

	values := convertValues(ag.Spec.Values)

	b, err := bundle.Build(packages, entrypointPkg, values, string(ag.Spec.Runtime), ag.Spec.Timeout)
	if err != nil {
		return nil, fmt.Errorf("building bundle: %w", err)
	}

	if err := m.store.Put(ag.Name, b); err != nil {
		return nil, fmt.Errorf("storing bundle: %w", err)
	}

	ns := ag.Spec.Target.Namespace
	name := GeneratedName(ag.Name)
	ownerRef := agentOwnerRef(ag)

	if err := m.ensureServiceAccount(ctx, name, ns, ownerRef); err != nil {
		return nil, fmt.Errorf("ensuring service account: %w", err)
	}
	if err := m.ensureRole(ctx, name, ns, ownerRef); err != nil {
		return nil, fmt.Errorf("ensuring role: %w", err)
	}
	if err := m.ensureRoleBinding(ctx, name, ns, ownerRef); err != nil {
		return nil, fmt.Errorf("ensuring role binding: %w", err)
	}
	if err := m.ensureClusterRole(ctx, name, ownerRef); err != nil {
		return nil, fmt.Errorf("ensuring cluster role: %w", err)
	}
	if err := m.ensureClusterRoleBinding(ctx, name, ns, ownerRef); err != nil {
		return nil, fmt.Errorf("ensuring cluster role binding: %w", err)
	}
	if err := m.ensureWorkloadOverride(ctx, ag, name, ownerRef, b.Digest); err != nil {
		return nil, fmt.Errorf("ensuring workload override: %w", err)
	}

	return &MaterializeResult{BundleDigest: b.Digest}, nil
}

func (m *Materializer) loadPackages(ctx context.Context, ag *v1alpha1.Agent) ([]collection.ScriptPackageContent, string, error) {
	var rc v1alpha1.ResourceCollection
	if err := m.client.Get(ctx, client.ObjectKey{Name: ag.Spec.Script.Collection}, &rc); err != nil {
		return nil, "", fmt.Errorf("reading collection %q: %w", ag.Spec.Script.Collection, err)
	}

	var collFS fs.FS
	if rc.Spec.Source != "" && m.collectionsDir != "" {
		var err error
		collFS, err = fs.Sub(os.DirFS(m.collectionsDir), rc.Spec.Source)
		if err != nil {
			return nil, "", fmt.Errorf("opening collection directory %q: %w", rc.Spec.Source, err)
		}
	}

	var packages []collection.ScriptPackageContent
	var entrypointPkg string

	for i := range rc.Spec.Scripts {
		pkg := &rc.Spec.Scripts[i]
		content, err := collection.LoadScriptPackage(pkg, collFS)
		if err != nil {
			return nil, "", fmt.Errorf("loading script package %q: %w", pkg.Name, err)
		}
		packages = append(packages, *content)
		if pkg.Name == ag.Spec.Script.Package {
			entrypointPkg = pkg.Name
		}
	}

	if entrypointPkg == "" {
		return nil, "", fmt.Errorf("package %q not found in collection %q", ag.Spec.Script.Package, ag.Spec.Script.Collection)
	}

	for _, refName := range rc.Spec.ScriptCollections {
		var ref v1alpha1.ResourceCollection
		if err := m.client.Get(ctx, client.ObjectKey{Name: refName}, &ref); err != nil {
			return nil, "", fmt.Errorf("reading referenced collection %q: %w", refName, err)
		}
		var refFS fs.FS
		if ref.Spec.Source != "" && m.collectionsDir != "" {
			var err error
			refFS, err = fs.Sub(os.DirFS(m.collectionsDir), ref.Spec.Source)
			if err != nil {
				return nil, "", fmt.Errorf("opening referenced collection directory %q: %w", ref.Spec.Source, err)
			}
		}
		for i := range ref.Spec.Scripts {
			pkg := &ref.Spec.Scripts[i]
			content, err := collection.LoadScriptPackage(pkg, refFS)
			if err != nil {
				return nil, "", fmt.Errorf("loading script package %q from collection %q: %w", pkg.Name, refName, err)
			}
			packages = append(packages, *content)
		}
	}

	return packages, entrypointPkg, nil
}

func (m *Materializer) ensureServiceAccount(ctx context.Context, name, namespace string, ownerRef metav1.OwnerReference) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	err := m.client.Create(ctx, sa)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (m *Materializer) ensureRole(ctx context.Context, name, namespace string, ownerRef metav1.OwnerReference) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Rules: defaultPolicyRules(),
	}
	err := m.client.Create(ctx, role)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (m *Materializer) ensureRoleBinding(ctx context.Context, name, namespace string, ownerRef metav1.OwnerReference) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      name,
			Namespace: namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		},
	}
	err := m.client.Create(ctx, rb)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (m *Materializer) ensureClusterRole(ctx context.Context, name string, ownerRef metav1.OwnerReference) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Rules: clusterScopedPolicyRules(),
	}
	err := m.client.Create(ctx, cr)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (m *Materializer) ensureClusterRoleBinding(ctx context.Context, name, namespace string, ownerRef metav1.OwnerReference) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      name,
			Namespace: namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     name,
		},
	}
	err := m.client.Create(ctx, crb)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (m *Materializer) ensureWorkloadOverride(ctx context.Context, ag *v1alpha1.Agent, name string, ownerRef metav1.OwnerReference, digest string) error {
	bundleURL := fmt.Sprintf("%s/bundles/%s", m.bundleBaseURL, digest)
	saName := name

	desired := &v1alpha1.WorkloadOverride{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: m.buildWOSpec(ag, bundleURL, digest, saName),
	}

	err := m.client.Create(ctx, desired)
	if apierrors.IsAlreadyExists(err) {
		var existing v1alpha1.WorkloadOverride
		if err := m.client.Get(ctx, client.ObjectKey{Name: name}, &existing); err != nil {
			return err
		}
		existing.Spec = desired.Spec
		existing.OwnerReferences = desired.OwnerReferences
		return m.client.Update(ctx, &existing)
	}
	return err
}

func (m *Materializer) buildWOSpec(ag *v1alpha1.Agent, bundleURL, digest, saName string) v1alpha1.WorkloadOverrideSpec {
	match := v1alpha1.WorkloadOverrideMatch{
		Namespace: ag.Spec.Target.Namespace,
		Labels:    ag.Spec.Target.Labels,
		PodName:   ag.Spec.Target.PodName,
		NodeName:  ag.Spec.Target.NodeName,
	}

	spec := v1alpha1.WorkloadOverrideSpec{
		Match:              match,
		ServiceAccountName: saName,
	}

	switch ag.Spec.Target.Replace {
	case v1alpha1.AgentReplaceContainer:
		// TODO: EnvOverride needs ValueFrom support so container override mode
		// can declare pod identity env vars the same way ReplacePod does.
		spec.Containers = []v1alpha1.ContainerOverride{{
			Name:    ag.Spec.Target.Container,
			Image:   m.agentImage,
			Command: []string{"/usr/bin/kit-agent"},
			Env: []v1alpha1.EnvOverride{
				{Name: "KIT_AGENT_BUNDLE_URL", Value: bundleURL},
				{Name: "KIT_AGENT_BUNDLE_SHA256", Value: digest},
			},
		}}
	default:
		spec.ReplacePod = &v1alpha1.PodReplacement{
			Name:    "kit-agent",
			Image:   m.agentImage,
			Command: []string{"/usr/bin/kit-agent"},
			Env: append(podIdentityEnvVars(), []corev1.EnvVar{
				{Name: "KIT_AGENT_BUNDLE_URL", Value: bundleURL},
				{Name: "KIT_AGENT_BUNDLE_SHA256", Value: digest},
			}...),
		}
	}

	return spec
}

// GeneratedName returns the deterministic name used for all resources
// materialized from an Agent (SA, Role, RoleBinding, WorkloadOverride).
func GeneratedName(agentName string) string {
	return "kit-agent-" + agentName
}

// IsWOFresh returns true when the WorkloadOverride is owned by the Agent,
// the reconciler has processed the current Agent generation with a
// successful materialization (Ready=True), and the bundle digest in the
// WO matches the Agent status.
func IsWOFresh(ag *v1alpha1.Agent, wo *v1alpha1.WorkloadOverride) bool {
	owned := false
	for _, ref := range wo.OwnerReferences {
		if ref.UID == ag.UID {
			owned = true
			break
		}
	}
	if !owned {
		return false
	}
	if ag.Status.ObservedGeneration != ag.Generation {
		return false
	}
	cond := meta.FindStatusCondition(ag.Status.Conditions, v1alpha1.ConditionAgentReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != ag.Generation {
		return false
	}
	if ag.Status.BundleDigest == "" {
		return false
	}
	return WODigest(wo) == ag.Status.BundleDigest
}

// WODigest extracts the bundle digest from a WorkloadOverride's
// KIT_AGENT_BUNDLE_SHA256 env var.
func WODigest(wo *v1alpha1.WorkloadOverride) string {
	if wo.Spec.ReplacePod != nil {
		for _, env := range wo.Spec.ReplacePod.Env {
			if env.Name == "KIT_AGENT_BUNDLE_SHA256" {
				return env.Value
			}
		}
	}
	for _, c := range wo.Spec.Containers {
		for _, env := range c.Env {
			if env.Name == "KIT_AGENT_BUNDLE_SHA256" {
				return env.Value
			}
		}
	}
	return ""
}

func podIdentityEnvVars() []corev1.EnvVar {
	fieldRef := func(path string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}
	}
	return []corev1.EnvVar{
		{Name: "KIT_POD_NAME", ValueFrom: fieldRef("metadata.name")},
		{Name: "KIT_POD_UID", ValueFrom: fieldRef("metadata.uid")},
		{Name: "KIT_NODE_NAME", ValueFrom: fieldRef("spec.nodeName")},
	}
}

func agentOwnerRef(ag *v1alpha1.Agent) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "Agent",
		Name:       ag.Name,
		UID:        ag.UID,
		Controller: ptr.To(true),
	}
}

func convertValues(src map[string]apiextensionsv1.JSON) map[string]json.RawMessage {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for k, v := range src {
		dst[k] = json.RawMessage(v.Raw)
	}
	return dst
}

func defaultPolicyRules() []rbacv1.PolicyRule {
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
// This must be replaced by a per-Agent RBAC spec (e.g. Agent.spec.rbac.clusterRules)
// so collections declare the access they need instead of granting a fixed superset.
func clusterScopedPolicyRules() []rbacv1.PolicyRule {
	readOnly := []string{"get", "list", "watch"}
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"nodes/status", "persistentvolumes", "namespaces"}, Verbs: readOnly},
		{APIGroups: []string{"kit.openshift.io"}, Resources: []string{"*"}, Verbs: readOnly},
		// TEST-ONLY: remove when Agent.spec.rbac is implemented
		{APIGroups: []string{"machineconfiguration.openshift.io"}, Resources: []string{"*"}, Verbs: readOnly},
		{APIGroups: []string{"config.openshift.io"}, Resources: []string{"*"}, Verbs: readOnly},
	}
}
