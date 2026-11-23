package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"text/template"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/cert"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	collcel "github.com/openshift-eng/kubernetes-integration-testing/internal/collection/cel"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/dynutil"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const collectionFinalizer = "kit.openshift.io/collection-protection"

// ScriptPackageLoader loads script packages and computes content hashes
// for a collection and its transitive dependencies.
type ScriptPackageLoader interface {
	Load(ctx context.Context, collectionName string, resolver collection.ScriptCollectionResolver) (*collection.LoadResult, error)
}

type collectionReconciler struct {
	client         client.Client
	clients        *kube.Clients
	release        *release.ReleaseManifests
	scriptLoader   ScriptPackageLoader
	collectionsDir string
	pullSecret     string
	certProvider   *cert.Provider
	log            *slog.Logger
}

func NewCollectionReconciler(c client.Client, log *slog.Logger, clients *kube.Clients, release *release.ReleaseManifests, scriptLoader ScriptPackageLoader, collectionsDir string, pullSecret string, certProvider *cert.Provider) *collectionReconciler {
	return &collectionReconciler{
		client:         c,
		clients:        clients,
		release:        release,
		scriptLoader:   scriptLoader,
		collectionsDir: collectionsDir,
		pullSecret:     pullSecret,
		certProvider:   certProvider,
		log:            log,
	}
}

func (r *collectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rc v1alpha1.ResourceCollection
	if err := r.client.Get(ctx, req.NamespacedName, &rc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !rc.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &rc)
	}

	if !controllerutil.ContainsFinalizer(&rc, collectionFinalizer) {
		controllerutil.AddFinalizer(&rc, collectionFinalizer)
		if err := r.client.Update(ctx, &rc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Rendering can create certificates and secrets, so collections that
	// nothing consumes are not rendered.
	planRefs, err := r.planRequestState(ctx, rc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	consumed := planRefs.referenced
	if !consumed {
		consumed, err = r.isReferencedByAgent(ctx, rc.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if !consumed {
		consumed, err = r.isReferencedByCollection(ctx, rc.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if !consumed {
		r.markNotReferenced(&rc)
		return ctrl.Result{}, nil
	}

	var kcList v1alpha1.KitClusterList
	if err := r.client.List(ctx, &kcList); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing KitCluster: %w", err)
	}
	if len(kcList.Items) == 0 {
		return ctrl.Result{}, fmt.Errorf("no KitCluster found")
	}
	kc := kcList.Items[0]

	if !meta.IsStatusConditionTrue(kc.Status.Conditions, v1alpha1.ConditionReady) {
		r.log.Info("KitCluster not ready, deferring collection reconcile", "name", rc.Name)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	hasApplyWork := hasCollectionApplyWork(&rc)
	prevApplied := meta.IsStatusConditionTrue(rc.Status.Conditions, v1alpha1.ConditionCollectionApplied)

	// Frozen: already applied and no live request. The last apply still
	// describes the cluster, and nothing reapplies until a plan asks
	// again. Leave the status untouched.
	if hasApplyWork && prevApplied && !planRefs.requested {
		return ctrl.Result{}, nil
	}

	renderResult, err := r.render(&rc, &kc)
	if err != nil {
		reason := "RenderFailed"
		if errors.Is(err, collection.ErrSourceMissing) {
			reason = "SourceMissing"
		}
		r.setContentFailure(&rc, reason, err.Error())
		return ctrl.Result{}, err
	}

	contentHash, err := r.computeContentHash(ctx, &rc, renderResult.Hash)
	if err != nil {
		reason := "HashFailed"
		if errors.Is(err, collection.ErrSourceMissing) {
			reason = "SourceMissing"
		}
		r.setContentFailure(&rc, reason, err.Error())
		return ctrl.Result{}, err
	}

	// The current content is live when the last successful apply recorded
	// this very content hash. Collections without apply work are
	// trivially live.
	applied := !hasApplyWork
	if hasApplyWork && planRefs.requested {
		if prevApplied && rc.Status.ContentHash == contentHash && rc.Status.ContentHash != "" {
			applied = true
		} else {
			r.log.Info("applying resource collection", "name", rc.Name, "oldHash", rc.Status.ContentHash, "newHash", contentHash)
			if err := r.apply(ctx, renderResult); err != nil {
				r.setApplyFailure(&rc, "ApplyFailed", err.Error())
				return ctrl.Result{}, err
			}

			if len(rc.Spec.StatusPatches) > 0 {
				if err := r.applyStatusPatches(ctx, &rc, &kc, renderResult); err != nil {
					r.setApplyFailure(&rc, "StatusPatchFailed", err.Error())
					return ctrl.Result{}, err
				}
			}
			applied = true
		}
	}

	desired := desiredCollectionConditions(&rc, hasApplyWork, planRefs.pending, applied)
	if collectionStatusCurrent(&rc, contentHash, desired) {
		return ctrl.Result{}, nil
	}

	base := rc.DeepCopy()
	rc.Status.ContentHash = contentHash
	for _, cond := range desired {
		meta.SetStatusCondition(&rc.Status.Conditions, cond)
	}
	if err := r.client.Status().Patch(ctx, &rc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	r.log.Info("reconciled resource collection", "name", rc.Name, "requested", planRefs.requested, "applied", applied)
	return ctrl.Result{}, nil
}

// planReferences summarizes how DeploymentPlans reference a collection
// by spec.
type planReferences struct {
	// referenced is true when any step names the collection, whatever
	// its phase.
	referenced bool
	// requested is true when a referencing step is in flight (Requested
	// or Failed).
	requested bool
	// pending is true when a referencing step has not been reached yet
	// (Pending, or no status).
	pending bool
}

// planRequestState reports how DeploymentPlans reference the collection.
func (r *collectionReconciler) planRequestState(ctx context.Context, name string) (planReferences, error) {
	var planList v1alpha1.DeploymentPlanList
	if err := r.client.List(ctx, &planList); err != nil {
		return planReferences{}, fmt.Errorf("listing DeploymentPlan: %w", err)
	}
	var refs planReferences
	for _, plan := range planList.Items {
		for i, step := range plan.Spec.Steps {
			if step.Collection == nil || step.Collection.Source != name {
				continue
			}
			refs.referenced = true
			if i >= len(plan.Status.Steps) {
				refs.pending = true
				continue
			}
			switch plan.Status.Steps[i].Phase {
			case v1alpha1.StepRequested, v1alpha1.StepFailed:
				refs.requested = true
			case v1alpha1.StepPending:
				refs.pending = true
			}
		}
	}
	return refs, nil
}

// hasCollectionApplyWork reports whether the collection declares content
// a DeploymentPlan can request to apply: templates or status patches.
func hasCollectionApplyWork(rc *v1alpha1.ResourceCollection) bool {
	return rc.Spec.Source != "" || len(rc.Spec.Inline) > 0 || len(rc.Spec.StatusPatches) > 0
}

// markNotReferenced stamps a collection that nothing consumes with
// Ready=Unknown so its lack of status is explicit. Transitional wait
// states (AppliedPlanPending, AppliedNotRequested) are also rewritten:
// their messages name consumers that no longer exist, which is reachable
// by creating and deleting plans on a live cluster. Anything else — in
// particular a successful last apply — is left untouched.
func (r *collectionReconciler) markNotReferenced(rc *v1alpha1.ResourceCollection) {
	ready := meta.FindStatusCondition(rc.Status.Conditions, v1alpha1.ConditionCollectionReady)
	if ready != nil &&
		ready.Reason != v1alpha1.ReasonCollectionAppliedPlanPending &&
		ready.Reason != v1alpha1.ReasonCollectionAppliedNotRequested {
		return
	}

	base := rc.DeepCopy()
	stampNotReferenced := func(condType string) {
		meta.SetStatusCondition(&rc.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionUnknown,
			Reason:             "NotReferenced",
			Message:            "no plan, agent, or collection references this collection",
			ObservedGeneration: rc.Generation,
		})
	}
	stampNotReferenced(v1alpha1.ConditionCollectionReady)
	if applied := meta.FindStatusCondition(rc.Status.Conditions, v1alpha1.ConditionCollectionApplied); applied != nil &&
		(applied.Reason == v1alpha1.ReasonCollectionAppliedPlanPending || applied.Reason == v1alpha1.ReasonCollectionAppliedNotRequested) {
		stampNotReferenced(v1alpha1.ConditionCollectionApplied)
	}
	if err := r.client.Status().Patch(context.Background(), rc, client.MergeFrom(base)); err != nil {
		r.log.Error("patching collection status", "name", rc.Name, "error", err)
	}
}

// desiredCollectionConditions computes the conditions describing the
// collection's own state after a successful render: ContentReady reflects
// content validity, Applied reports whether the apply work (templates and
// status patches) is live on the cluster — Unknown while no plan has
// requested it — and Ready aggregates both.
func desiredCollectionConditions(rc *v1alpha1.ResourceCollection, hasApplyWork, planPending, applied bool) []metav1.Condition {
	conds := []metav1.Condition{{
		Type:               v1alpha1.ConditionCollectionContentReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		ObservedGeneration: rc.Generation,
	}}

	appliedCond := metav1.Condition{
		Type:               v1alpha1.ConditionCollectionApplied,
		ObservedGeneration: rc.Generation,
	}
	readyCond := metav1.Condition{
		Type:               v1alpha1.ConditionCollectionReady,
		ObservedGeneration: rc.Generation,
	}
	switch {
	case applied:
		if !hasApplyWork {
			appliedCond.Status, appliedCond.Reason = metav1.ConditionTrue, "NothingToApply"
			appliedCond.Message = "collection has nothing to apply"
		} else {
			appliedCond.Status, appliedCond.Reason = metav1.ConditionTrue, "Applied"
		}
		readyCond.Status, readyCond.Reason = metav1.ConditionTrue, "Ready"
	case planPending:
		appliedCond.Status, appliedCond.Reason = metav1.ConditionUnknown, v1alpha1.ReasonCollectionAppliedPlanPending
		appliedCond.Message = "referenced by a DeploymentPlan step that has not been reached yet"
		readyCond.Status, readyCond.Reason = metav1.ConditionUnknown, v1alpha1.ReasonCollectionAppliedPlanPending
		readyCond.Message = appliedCond.Message
	default:
		appliedCond.Status, appliedCond.Reason = metav1.ConditionUnknown, v1alpha1.ReasonCollectionAppliedNotRequested
		appliedCond.Message = "no DeploymentPlan has requested this collection"
		readyCond.Status, readyCond.Reason = metav1.ConditionUnknown, v1alpha1.ReasonCollectionAppliedNotRequested
		readyCond.Message = appliedCond.Message
	}
	return append(conds, appliedCond, readyCond)
}

// collectionStatusCurrent reports whether the stored status already
// describes the desired conditions for contentHash, so the reconciler can
// skip the status write (and the reconcile it would trigger).
func collectionStatusCurrent(rc *v1alpha1.ResourceCollection, contentHash string, desired []metav1.Condition) bool {
	if contentHash == "" || rc.Status.ContentHash != contentHash {
		return false
	}
	for _, want := range desired {
		got := meta.FindStatusCondition(rc.Status.Conditions, want.Type)
		if got == nil {
			return false
		}
		if got.Status != want.Status || got.Reason != want.Reason ||
			got.Message != want.Message || got.ObservedGeneration != want.ObservedGeneration {
			return false
		}
	}
	return true
}

// setContentFailure records a content failure (render or hash): the Ready
// aggregate mirrors it and Applied is marked False so a requesting
// DeploymentPlan step fails fast with the content error instead of
// waiting. ContentHash is left untouched: it still describes the last
// content that rendered successfully.
func (r *collectionReconciler) setContentFailure(rc *v1alpha1.ResourceCollection, reason, message string) {
	base := rc.DeepCopy()
	for _, condType := range []string{
		v1alpha1.ConditionCollectionContentReady,
		v1alpha1.ConditionCollectionReady,
		v1alpha1.ConditionCollectionApplied,
	} {
		meta.SetStatusCondition(&rc.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: rc.Generation,
		})
	}
	if err := r.client.Status().Patch(context.Background(), rc, client.MergeFrom(base)); err != nil {
		r.log.Error("patching collection status", "name", rc.Name, "error", err)
	}
}

// setApplyFailure records an apply failure: content is valid but not
// live on the cluster.
func (r *collectionReconciler) setApplyFailure(rc *v1alpha1.ResourceCollection, reason, message string) {
	base := rc.DeepCopy()
	meta.SetStatusCondition(&rc.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionCollectionContentReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		ObservedGeneration: rc.Generation,
	})
	for _, condType := range []string{
		v1alpha1.ConditionCollectionApplied,
		v1alpha1.ConditionCollectionReady,
	} {
		meta.SetStatusCondition(&rc.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: rc.Generation,
		})
	}
	if err := r.client.Status().Patch(context.Background(), rc, client.MergeFrom(base)); err != nil {
		r.log.Error("patching collection status", "name", rc.Name, "error", err)
	}
}

func (r *collectionReconciler) render(rc *v1alpha1.ResourceCollection, kc *v1alpha1.KitCluster) (*collection.RenderResult, error) {
	coll, err := r.loadCollection(rc)
	if err != nil {
		return nil, err
	}

	values, err := unmarshalValues(rc.Spec.Values)
	if err != nil {
		return nil, err
	}

	// TODO: Platform and NetworkType should come from KitCluster spec.
	renderCtx := collection.RenderContext{
		FeatureSet:   kc.Spec.FeatureSet,
		APIServerURL: kc.Status.APIServerURL,
		Platform:     "None",
		NetworkType:  "OVNKubernetes",
		DefaultCA:    kc.Status.DefaultCA,
		PullSecret:   r.pullSecret,
		Values:       values,
	}

	return coll.Render(renderCtx)
}

func (r *collectionReconciler) newMapper() (meta.RESTMapper, error) {
	groupResources, err := restmapper.GetAPIGroupResources(r.clients.Discovery)
	if err != nil {
		return nil, fmt.Errorf("discovering API resources: %w", err)
	}
	return restmapper.NewDiscoveryRESTMapper(groupResources), nil
}

// TODO: implement proper resource ordering (namespaces before namespaced
// resources, CRDs before CRs, etc.) instead of relying on template walk order.
func (r *collectionReconciler) apply(ctx context.Context, result *collection.RenderResult) error {
	if len(result.Objects) == 0 {
		return nil
	}

	mapper, err := r.newMapper()
	if err != nil {
		return err
	}

	var namespaces, rest []*unstructured.Unstructured
	for _, obj := range result.Objects {
		if obj.GetKind() == "Namespace" {
			namespaces = append(namespaces, obj)
		} else {
			rest = append(rest, obj)
		}
	}

	for _, obj := range append(namespaces, rest...) {
		applied, err := dynutil.ApplyWithMapper(ctx, r.clients.Dynamic, mapper, obj)
		if err != nil {
			return fmt.Errorf("applying %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		r.log.Info("applied collection resource", "kind", applied.GetKind(), "name", applied.GetName())
	}
	return nil
}

func (r *collectionReconciler) applyStatusPatches(ctx context.Context, rc *v1alpha1.ResourceCollection, kc *v1alpha1.KitCluster, rendered *collection.RenderResult) error {
	var releaseObjs []map[string]interface{}
	if r.release != nil {
		manifests, err := r.release.All()
		if err != nil {
			return fmt.Errorf("loading release manifests: %w", err)
		}
		releaseObjs = make([]map[string]interface{}, len(manifests))
		for i, m := range manifests {
			releaseObjs[i] = m.Obj.Object
		}
	}

	patches := make([]collcel.StatusPatchInput, len(rc.Spec.StatusPatches))
	for i, sp := range rc.Spec.StatusPatches {
		patches[i] = collcel.StatusPatchInput{Kind: sp.Kind, Name: sp.Name, Status: sp.Status}
	}

	values, err := unmarshalValues(rc.Spec.Values)
	if err != nil {
		return err
	}

	results, err := collcel.EvalStatusPatches(collcel.Context{
		Cluster: collcel.ClusterContext{
			Platform:     "None",
			APIServerURL: kc.Status.APIServerURL,
			FeatureSet:   kc.Spec.FeatureSet,
			Version:      r.release.Version(),
			Image:        kc.Spec.Image,
		},
		Values:  values,
		Release: releaseObjs,
	}, patches)
	if err != nil {
		return err
	}

	mapper, err := r.newMapper()
	if err != nil {
		return err
	}

	for _, sr := range results {
		target := findRenderedObject(rendered, sr.Kind, sr.Name)
		if target == nil {
			return fmt.Errorf("status patch target %s/%s not found in rendered objects", sr.Kind, sr.Name)
		}
		if err := dynutil.UpdateStatus(ctx, r.clients.Dynamic, mapper, target, sr.Status); err != nil {
			return fmt.Errorf("status patch %s/%s: %w", sr.Kind, sr.Name, err)
		}
		r.log.Info("applied status patch", "kind", sr.Kind, "name", sr.Name)
	}
	return nil
}

func findRenderedObject(result *collection.RenderResult, kind, name string) *unstructured.Unstructured {
	for _, obj := range result.Objects {
		if obj.GetKind() == kind && obj.GetName() == name {
			return obj
		}
	}
	return nil
}

func (r *collectionReconciler) certFuncMap() template.FuncMap {
	return template.FuncMap{
		"caCert": r.certProvider.CACert,
		"signedCert": func(caName, cn string) (string, error) {
			return r.certProvider.SignedCert(caName, cn)
		},
		"signedKey": func(caName, cn string) (string, error) {
			return r.certProvider.SignedKey(caName, cn)
		},
	}
}

func (r *collectionReconciler) loadCollection(rc *v1alpha1.ResourceCollection) (*collection.Collection, error) {
	if len(rc.Spec.Inline) > 0 {
		contents := make([]string, len(rc.Spec.Inline))
		for i, t := range rc.Spec.Inline {
			contents[i] = t.Content
		}
		return collection.LoadInline(rc.Name, contents, r.certFuncMap())
	}

	if rc.Spec.Source == "" {
		return collection.NewEmpty(rc.Name), nil
	}

	var userFS fs.FS
	if r.collectionsDir != "" {
		userFS = os.DirFS(r.collectionsDir)
	}
	return collection.Load(rc.Spec.Source, userFS, r.certFuncMap())
}

func (r *collectionReconciler) handleDeletion(ctx context.Context, rc *v1alpha1.ResourceCollection) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rc, collectionFinalizer) {
		return ctrl.Result{}, nil
	}

	requested, err := r.isReferencedByPlan(ctx, rc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requested {
		r.log.Info("collection still requested by DeploymentPlan, blocking deletion", "name", rc.Name)
		return ctrl.Result{}, nil
	}

	isReferencedByAgent, err := r.isReferencedByAgent(ctx, rc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if isReferencedByAgent {
		r.log.Info("collection still referenced by Agent, blocking deletion", "name", rc.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	referenced, err := r.isReferencedByCollection(ctx, rc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if referenced {
		r.log.Info("collection still referenced by another ResourceCollection via scriptCollections, blocking deletion", "name", rc.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(rc, collectionFinalizer)
	if err := r.client.Update(ctx, rc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *collectionReconciler) isReferencedByCollection(ctx context.Context, name string) (bool, error) {
	var list v1alpha1.ResourceCollectionList
	if err := r.client.List(ctx, &list); err != nil {
		return false, fmt.Errorf("listing ResourceCollections: %w", err)
	}
	for _, other := range list.Items {
		// Collections that are flagged to be deleted are not considered
		if other.Name == name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		for _, ref := range other.Spec.ScriptCollections {
			if ref == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// isReferencedByPlan reports whether any live plan references the
// collection from an in-flight step (Requested or Failed). Used for
// deletion protection: completed (Ready) steps do not count, so a
// finished plan does not keep its collections undeletable.
func (r *collectionReconciler) isReferencedByPlan(ctx context.Context, name string) (bool, error) {
	var planList v1alpha1.DeploymentPlanList
	if err := r.client.List(ctx, &planList); err != nil {
		return false, fmt.Errorf("listing DeploymentPlan: %w", err)
	}
	for _, plan := range planList.Items {
		for i, stepStatus := range plan.Status.Steps {
			if stepStatus.Phase != v1alpha1.StepRequested && stepStatus.Phase != v1alpha1.StepFailed {
				continue
			}
			if i < len(plan.Spec.Steps) && plan.Spec.Steps[i].Collection != nil && plan.Spec.Steps[i].Collection.Source == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// isReferencedByAgent reports whether any live Agent resolves scripts
// from the collection. Used by the consumption filter and for deletion
// protection.
func (r *collectionReconciler) isReferencedByAgent(ctx context.Context, name string) (bool, error) {
	var agentList v1alpha1.AgentList
	if err := r.client.List(ctx, &agentList); err != nil {
		return false, fmt.Errorf("listing Agent: %w", err)
	}
	for _, agent := range agentList.Items {
		if agent.Spec.Script.Collection == name {
			return true, nil
		}
	}
	return false, nil
}

func (r *collectionReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	bldr := ctrl.NewControllerManagedBy(mgr).
		Named("resourcecollection").
		For(&v1alpha1.ResourceCollection{}).
		Watches(&v1alpha1.DeploymentPlan{}, handler.EnqueueRequestsFromMapFunc(r.planToCollections)).
		Watches(&v1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.agentToCollection),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.ResourceCollection{}, handler.EnqueueRequestsFromMapFunc(r.collectionScriptMapping))

	for _, name := range collection.BuiltinNames() {
		fsys, err := collection.BuiltinFS(name)
		if err != nil {
			r.log.Warn("builtin collection FS not found", "name", name, "error", err)
			continue
		}
		if err := r.ensureResourceCollection(ctx, name, fsys); err != nil {
			r.log.Warn("failed to auto-create builtin ResourceCollection", "name", name, "error", err)
		}
	}

	if r.collectionsDir != "" {
		if err := r.ensureCollectionsFromDisk(ctx); err != nil {
			r.log.Warn("failed to scan collections dir on startup", "error", err)
		}

		ch, err := r.startFSBridge(ctx)
		if err != nil {
			r.log.Warn("filesystem watcher not available, changes on disk won't trigger reconcile", "error", err)
		} else {
			bldr = bldr.WatchesRawSource(source.Channel(ch, &handler.EnqueueRequestForObject{}))
		}
	}

	return bldr.Complete(r)
}

// planToCollections enqueues every collection a plan references, so a
// collection becomes evaluated (and later requested) as soon as a plan
// names it, whatever the step phase.
func (r *collectionReconciler) planToCollections(_ context.Context, obj client.Object) []ctrl.Request {
	plan, ok := obj.(*v1alpha1.DeploymentPlan)
	if !ok {
		return nil
	}
	var requests []ctrl.Request
	for _, step := range plan.Spec.Steps {
		if step.Collection != nil {
			requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: step.Collection.Source}})
		}
	}
	return requests
}

// agentToCollection enqueues the collection an Agent resolves scripts
// from, so collections consumed only by agents are evaluated when the
// Agent appears.
func (r *collectionReconciler) agentToCollection(_ context.Context, obj client.Object) []ctrl.Request {
	agent, ok := obj.(*v1alpha1.Agent)
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: agent.Spec.Script.Collection}}}
}

// collectionScriptMapping enqueues both directions when a collection changes:
// its own dependencies (so they can release finalizers) and all transitive
// dependents (so they recalculate ContentHash when a dependency's content changes).
func (r *collectionReconciler) collectionScriptMapping(ctx context.Context, obj client.Object) []ctrl.Request {
	changed, ok := obj.(*v1alpha1.ResourceCollection)
	if !ok {
		return nil
	}

	var list v1alpha1.ResourceCollectionList
	if err := r.client.List(ctx, &list); err != nil {
		r.log.Error("listing collections for script mapping", "error", err)
		return nil
	}

	deps := buildDependents(list.Items)
	seen := map[string]bool{}
	var requests []ctrl.Request
	for _, name := range transitiveDependents(changed.Name, deps) {
		seen[name] = true
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
	}

	// Downward: own dependencies (so they can release finalizers).
	for _, ref := range changed.Spec.ScriptCollections {
		if !seen[ref] {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: ref}})
		}
	}
	return requests
}

// buildDependents builds the inverse graph: for each collection name,
// which collections list it in their scriptCollections.
func buildDependents(collections []v1alpha1.ResourceCollection) map[string][]string {
	deps := make(map[string][]string)
	for _, rc := range collections {
		for _, ref := range rc.Spec.ScriptCollections {
			deps[ref] = append(deps[ref], rc.Name)
		}
	}
	return deps
}

// transitiveDependents returns all collection names that transitively
// depend on name via the inverse graph. Does not include name itself.
func transitiveDependents(name string, deps map[string][]string) []string {
	seen := map[string]bool{name: true}
	queue := append([]string{}, deps[name]...)
	var result []string
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		result = append(result, current)
		queue = append(queue, deps[current]...)
	}
	return result
}

func (r *collectionReconciler) ensureCollectionsFromDisk(ctx context.Context) error {
	entries, err := os.ReadDir(r.collectionsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		has, err := hasTemplates(filepath.Join(r.collectionsDir, entry.Name()))
		if err != nil {
			r.log.Warn("checking templates", "name", entry.Name(), "error", err)
			continue
		}
		if !has {
			continue
		}
		fsys := os.DirFS(filepath.Join(r.collectionsDir, entry.Name()))
		if err := r.ensureResourceCollection(ctx, entry.Name(), fsys); err != nil {
			r.log.Warn("failed to auto-create ResourceCollection", "name", entry.Name(), "error", err)
		}
	}
	return nil
}

func (r *collectionReconciler) ensureResourceCollection(ctx context.Context, name string, fsys fs.FS) error {
	spec := collection.LoadSpec(name, fsys)
	if spec == nil {
		spec = &v1alpha1.ResourceCollectionSpec{}
	}
	spec.Source = name

	rc := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       *spec,
	}
	if err := r.client.Create(ctx, rc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	r.log.Info("auto-created ResourceCollection", "name", name)
	return nil
}

func hasTemplates(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(d.Name())
		if ext == ".yaml" || ext == ".yml" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

func (r *collectionReconciler) tryAutoCreate(ctx context.Context, name string) {
	dir := filepath.Join(r.collectionsDir, name)
	has, err := hasTemplates(dir)
	if err != nil {
		r.log.Warn("checking templates for auto-create", "name", name, "error", err)
		return
	}
	if !has {
		return
	}
	fsys := os.DirFS(dir)
	if err := r.ensureResourceCollection(ctx, name, fsys); err != nil {
		r.log.Warn("failed to auto-create ResourceCollection from fs event", "name", name, "error", err)
	}
}

func (r *collectionReconciler) enqueueDependents(ctx context.Context, ch chan<- event.GenericEvent, name string) {
	var list v1alpha1.ResourceCollectionList
	if err := r.client.List(ctx, &list); err != nil {
		r.log.Error("listing collections for FS propagation", "error", err)
		return
	}
	deps := buildDependents(list.Items)
	byName := make(map[string]*v1alpha1.ResourceCollection, len(list.Items))
	for i := range list.Items {
		byName[list.Items[i].Name] = &list.Items[i]
	}
	for _, depName := range transitiveDependents(name, deps) {
		rc, ok := byName[depName]
		if !ok {
			continue
		}
		select {
		case ch <- event.GenericEvent{Object: rc}:
		case <-ctx.Done():
			return
		}
	}
}

func (r *collectionReconciler) startFSBridge(ctx context.Context) (<-chan event.GenericEvent, error) {
	w := collection.NewFSWatcher(r.collectionsDir, r.log, 100*time.Millisecond)
	fsCh, err := w.Start(ctx)
	if err != nil {
		return nil, err
	}

	ch := make(chan event.GenericEvent)
	go func() {
		defer close(ch)
		for fsEv := range fsCh {
			switch fsEv.Kind {
			case collection.FSEventCreate:
				r.tryAutoCreate(ctx, fsEv.Collection)
				continue

			case collection.FSEventRemove:
				var rc v1alpha1.ResourceCollection
				if err := r.client.Get(ctx, client.ObjectKey{Name: fsEv.Collection}, &rc); err != nil {
					continue
				}
				if rc.Spec.Source == "" {
					continue
				}
				select {
				case ch <- event.GenericEvent{Object: &rc}:
				case <-ctx.Done():
					return
				}
				r.enqueueDependents(ctx, ch, fsEv.Collection)
				continue

			case collection.FSEventChange:
				var rc v1alpha1.ResourceCollection
				if err := r.client.Get(ctx, client.ObjectKey{Name: fsEv.Collection}, &rc); err != nil {
					if apierrors.IsNotFound(err) {
						r.tryAutoCreate(ctx, fsEv.Collection)
					}
					continue
				}
				if rc.Spec.Source == "" {
					continue
				}
				select {
				case ch <- event.GenericEvent{Object: &rc}:
				case <-ctx.Done():
					return
				}
				r.enqueueDependents(ctx, ch, fsEv.Collection)
			}
		}
	}()
	return ch, nil
}

func (r *collectionReconciler) computeContentHash(ctx context.Context, rc *v1alpha1.ResourceCollection, renderHash string) (string, error) {
	scriptHash := collection.EmptyHash
	if len(rc.Spec.Scripts) > 0 || len(rc.Spec.ScriptCollections) > 0 {
		loaded, err := r.scriptLoader.Load(ctx, rc.Name, &collectionResolver{r.client})
		if err != nil {
			return "", fmt.Errorf("computing script hash for %q: %w", rc.Name, err)
		}
		scriptHash = loaded.ContentHash
	}

	statusPatchesHash, err := collection.HashStatusPatches(rc.Spec.StatusPatches)
	if err != nil {
		return "", fmt.Errorf("hashing status patches for %q: %w", rc.Name, err)
	}

	return collection.CombineContentHash(collection.ContentHashComponents{
		RenderHash:        renderHash,
		ScriptHash:        scriptHash,
		StatusPatchesHash: statusPatchesHash,
	})
}

type collectionResolver struct {
	c client.Client
}

func (r *collectionResolver) Resolve(ctx context.Context, name string) (*v1alpha1.ResourceCollection, error) {
	var rc v1alpha1.ResourceCollection
	if err := r.c.Get(ctx, client.ObjectKey{Name: name}, &rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

func unmarshalValues(raw map[string]apiextensionsv1.JSON) (map[string]any, error) {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		var parsed any
		if err := json.Unmarshal(v.Raw, &parsed); err != nil {
			return nil, fmt.Errorf("unmarshaling value %q: %w", k, err)
		}
		out[k] = parsed
	}
	return out, nil
}
