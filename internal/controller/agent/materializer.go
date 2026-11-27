package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	agentrbac "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent/rbac"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ScriptPackageLoader interface {
	Load(ctx context.Context, collectionName string, resolver collection.ScriptCollectionResolver) (*collection.LoadResult, error)
}

type Materializer struct {
	client         client.Client
	store          *bundle.BundleStore
	scriptLoader   ScriptPackageLoader
	scopeValidator *agentrbac.ScopeValidator
	agentImage     string
	bundleBaseURL  string
	log            *slog.Logger
}

type MaterializeResult struct {
	BundleDigest string
}

func NewMaterializer(c client.Client, store *bundle.BundleStore, loader ScriptPackageLoader, scopeValidator *agentrbac.ScopeValidator, agentImage, bundleBaseURL string, log *slog.Logger) *Materializer {
	return &Materializer{
		client:         c,
		store:          store,
		scriptLoader:   loader,
		scopeValidator: scopeValidator,
		agentImage:     agentImage,
		bundleBaseURL:  bundleBaseURL,
		log:            log,
	}
}

func (m *Materializer) Materialize(ctx context.Context, ag *v1alpha1.Agent) (*MaterializeResult, error) {
	loaded, err := m.scriptLoader.Load(ctx, ag.Spec.Script.Collection, &clientResolver{m.client})
	if err != nil {
		return nil, err
	}

	if !loaded.LocalPackages[ag.Spec.Script.Package] {
		return nil, fmt.Errorf("entrypoint package %q is not in collection %q", ag.Spec.Script.Package, ag.Spec.Script.Collection)
	}

	if err := m.scopeValidator.Validate(ctx, loaded.EffectiveCapabilities); err != nil {
		return nil, err
	}

	values := convertValues(ag.Spec.Values)

	b, err := bundle.Build(bundle.BuildInput{
		Packages:              loaded.Packages,
		EntrypointPackage:     ag.Spec.Script.Package,
		Values:                values,
		Runtime:               string(ag.Spec.Runtime),
		Timeout:               ag.Spec.Timeout,
		EffectiveCapabilities: loaded.EffectiveCapabilities,
	})
	if err != nil {
		return nil, fmt.Errorf("building bundle: %w", err)
	}

	if err := m.store.Put(ag.Name, b); err != nil {
		return nil, fmt.Errorf("storing bundle: %w", err)
	}

	ns := ag.Spec.Target.Namespace
	name := GeneratedName(ag.Name)
	ownerRef := agentOwnerRef(ag)

	rbacObjects := agentrbac.Compile(agentrbac.CompileInput{
		Name:                  name,
		TargetNamespace:       ns,
		OwnerRef:              ownerRef,
		EffectiveCapabilities: loaded.EffectiveCapabilities,
	})

	desiredSet := desiredObjectSet(name, ns, rbacObjects)

	if err := m.ensureIdentityAndRBAC(ctx, name, ns, ownerRef, rbacObjects); err != nil {
		return nil, err
	}

	if err := m.ensureWorkloadOverride(ctx, ag, name, ownerRef, b.Digest); err != nil {
		return nil, fmt.Errorf("ensuring workload override: %w", err)
	}

	if err := m.pruneStaleRBACObjects(ctx, ownerRef.UID, desiredSet); err != nil {
		return nil, fmt.Errorf("pruning stale objects: %w", err)
	}

	return &MaterializeResult{BundleDigest: b.Digest}, nil
}

// clientResolver adapts a controller-runtime client to ScriptCollectionResolver.
type clientResolver struct {
	c client.Client
}

func (r *clientResolver) Resolve(ctx context.Context, name string) (*v1alpha1.ResourceCollection, error) {
	var rc v1alpha1.ResourceCollection
	if err := r.c.Get(ctx, client.ObjectKey{Name: name}, &rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

func (m *Materializer) ensureIdentityAndRBAC(ctx context.Context, name, ns string, ownerRef metav1.OwnerReference, rbacObjects []client.Object) error {
	sa := buildServiceAccount(name, ns, ownerRef)
	if err := applyObject(ctx, m.client, sa); err != nil {
		return fmt.Errorf("ensuring service account: %w", err)
	}
	for _, obj := range rbacObjects {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		if err := applyObject(ctx, m.client, obj); err != nil {
			return fmt.Errorf("applying %s %s/%s: %w",
				kind, obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

func buildServiceAccount(name, namespace string, ownerRef metav1.OwnerReference) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          map[string]string{agentrbac.LabelAgentUID: string(ownerRef.UID)},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
}

func isOwnedBy(obj client.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

type objectID struct{ kind, namespace, name string }

// desiredObjectSet snapshots the kind/namespace/name of each object
// before SSA apply mutates them (Patch clears TypeMeta).
func desiredObjectSet(saName, saNS string, rbacObjects []client.Object) map[objectID]struct{} {
	current := map[objectID]struct{}{
		{"ServiceAccount", saNS, saName}: {},
	}
	for _, obj := range rbacObjects {
		current[objectID{
			kind:      obj.GetObjectKind().GroupVersionKind().Kind,
			namespace: obj.GetNamespace(),
			name:      obj.GetName(),
		}] = struct{}{}
	}
	return current
}

// pruneStaleRBACObjects deletes identity and RBAC objects that carry the
// agent-uid label and owner reference but are no longer in the desired
// set (e.g. after a namespace retarget or capability reduction). It runs
// after the WO apply so a WO failure does not leave the old WO pointing
// at a deleted SA.
func (m *Materializer) pruneStaleRBACObjects(ctx context.Context, agentUID types.UID, desiredSet map[objectID]struct{}) error {
	isStale := func(kind, namespace, name string) bool {
		_, ok := desiredSet[objectID{kind, namespace, name}]
		return !ok
	}

	selector := client.MatchingLabels{agentrbac.LabelAgentUID: string(agentUID)}

	var sas corev1.ServiceAccountList
	if err := m.client.List(ctx, &sas, selector); err != nil {
		return fmt.Errorf("listing service accounts: %w", err)
	}
	for i := range sas.Items {
		if isStale("ServiceAccount", sas.Items[i].Namespace, sas.Items[i].Name) && isOwnedBy(&sas.Items[i], agentUID) {
			if err := m.client.Delete(ctx, &sas.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting stale service account %s/%s: %w", sas.Items[i].Namespace, sas.Items[i].Name, err)
			}
		}
	}

	var roles rbacv1.RoleList
	if err := m.client.List(ctx, &roles, selector); err != nil {
		return fmt.Errorf("listing roles: %w", err)
	}
	for i := range roles.Items {
		if isStale("Role", roles.Items[i].Namespace, roles.Items[i].Name) && isOwnedBy(&roles.Items[i], agentUID) {
			if err := m.client.Delete(ctx, &roles.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting stale role %s/%s: %w", roles.Items[i].Namespace, roles.Items[i].Name, err)
			}
		}
	}

	var rbs rbacv1.RoleBindingList
	if err := m.client.List(ctx, &rbs, selector); err != nil {
		return fmt.Errorf("listing role bindings: %w", err)
	}
	for i := range rbs.Items {
		if isStale("RoleBinding", rbs.Items[i].Namespace, rbs.Items[i].Name) && isOwnedBy(&rbs.Items[i], agentUID) {
			if err := m.client.Delete(ctx, &rbs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting stale role binding %s/%s: %w", rbs.Items[i].Namespace, rbs.Items[i].Name, err)
			}
		}
	}

	var crs rbacv1.ClusterRoleList
	if err := m.client.List(ctx, &crs, selector); err != nil {
		return fmt.Errorf("listing cluster roles: %w", err)
	}
	for i := range crs.Items {
		if isStale("ClusterRole", crs.Items[i].Namespace, crs.Items[i].Name) && isOwnedBy(&crs.Items[i], agentUID) {
			if err := m.client.Delete(ctx, &crs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting stale cluster role %s: %w", crs.Items[i].Name, err)
			}
		}
	}

	var crbs rbacv1.ClusterRoleBindingList
	if err := m.client.List(ctx, &crbs, selector); err != nil {
		return fmt.Errorf("listing cluster role bindings: %w", err)
	}
	for i := range crbs.Items {
		if isStale("ClusterRoleBinding", crbs.Items[i].Namespace, crbs.Items[i].Name) && isOwnedBy(&crbs.Items[i], agentUID) {
			if err := m.client.Delete(ctx, &crbs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting stale cluster role binding %s: %w", crbs.Items[i].Name, err)
			}
		}
	}

	return nil
}

func (m *Materializer) ensureWorkloadOverride(ctx context.Context, ag *v1alpha1.Agent, name string, ownerRef metav1.OwnerReference, digest string) error {
	bundleURL := fmt.Sprintf("%s/bundles/%s", m.bundleBaseURL, digest)
	saName := name

	desired := &v1alpha1.WorkloadOverride{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "WorkloadOverride",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: m.buildWOSpec(ag, bundleURL, digest, saName),
	}

	return applyObject(ctx, m.client, desired)
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
			Name:       ag.Spec.Target.Container,
			Image:      m.agentImage,
			PullPolicy: corev1.PullIfNotPresent,
			Command:    []string{"/usr/bin/kit-agent"},
			Env: []v1alpha1.EnvOverride{
				{Name: "KIT_AGENT_BUNDLE_URL", Value: bundleURL},
				{Name: "KIT_AGENT_BUNDLE_SHA256", Value: digest},
			},
		}}
	default:
		spec.ReplacePod = &v1alpha1.PodReplacement{
			Name:       "kit-agent",
			Image:      m.agentImage,
			PullPolicy: corev1.PullIfNotPresent,
			Command:    []string{"/usr/bin/kit-agent"},
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

func applyObject(ctx context.Context, c client.Client, obj client.Object) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshaling for server-side apply: %w", err)
	}
	patch := client.RawPatch(types.ApplyPatchType, data)
	return c.Patch(ctx, obj, patch, client.FieldOwner(agentrbac.FieldManager), client.ForceOwnership)
}
