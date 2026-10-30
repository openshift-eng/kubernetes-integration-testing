package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	collcel "github.com/openshift-eng/kubernetes-integration-testing/internal/collection/cel"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/dynutil"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"

	"github.com/fsnotify/fsnotify"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const collectionFinalizer = "kit.openshift.io/collection-protection"

type collectionReconciler struct {
	client         client.Client
	dynClient      dynamic.Interface
	discovery      discovery.DiscoveryInterface
	release        *release.ReleaseManifests
	collectionsDir string
	pullSecret     string
	funcMap        template.FuncMap
	log            *slog.Logger
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

	requested, err := r.isRequested(ctx, rc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !requested {
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
		r.log.Info("KitCluster not ready, deferring collection apply", "name", rc.Name)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	renderResult, err := r.render(&rc, &kc)
	if err != nil {
		r.setCollectionCondition(&rc, metav1.ConditionFalse, "RenderFailed", err.Error())
		return ctrl.Result{}, err
	}

	if rc.Status.ContentHash == renderResult.Hash &&
		rc.Status.ContentHash != "" {
		appliedCond := meta.FindStatusCondition(rc.Status.Conditions, v1alpha1.ConditionCollectionApplied)
		if appliedCond != nil && appliedCond.Status == metav1.ConditionTrue &&
			appliedCond.ObservedGeneration == rc.Generation {
			return ctrl.Result{}, nil
		}
	}

	r.log.Info("applying resource collection", "name", rc.Name, "oldHash", rc.Status.ContentHash, "newHash", renderResult.Hash)
	if err := r.apply(ctx, renderResult); err != nil {
		r.setCollectionCondition(&rc, metav1.ConditionFalse, "ApplyFailed", err.Error())
		return ctrl.Result{}, err
	}

	if len(rc.Spec.StatusPatches) > 0 {
		if err := r.applyStatusPatches(ctx, &rc, &kc, renderResult); err != nil {
			r.setCollectionCondition(&rc, metav1.ConditionFalse, "StatusPatchFailed", err.Error())
			return ctrl.Result{}, err
		}
	}

	base := rc.DeepCopy()
	rc.Status.ContentHash = renderResult.Hash
	meta.SetStatusCondition(&rc.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionCollectionApplied,
		Status:             metav1.ConditionTrue,
		Reason:             "Applied",
		ObservedGeneration: rc.Generation,
	})
	if err := r.client.Status().Patch(ctx, &rc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	r.log.Info("applied resource collection", "name", rc.Name)
	return ctrl.Result{}, nil
}

func (r *collectionReconciler) setCollectionCondition(rc *v1alpha1.ResourceCollection, status metav1.ConditionStatus, reason, message string) {
	base := rc.DeepCopy()
	meta.SetStatusCondition(&rc.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionCollectionApplied,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rc.Generation,
	})
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
	groupResources, err := restmapper.GetAPIGroupResources(r.discovery)
	if err != nil {
		return nil, fmt.Errorf("discovering API resources: %w", err)
	}
	return restmapper.NewDiscoveryRESTMapper(groupResources), nil
}

// TODO: implement proper resource ordering (namespaces before namespaced
// resources, CRDs before CRs, etc.) instead of relying on template walk order.
func (r *collectionReconciler) apply(ctx context.Context, result *collection.RenderResult) error {
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
		applied, err := dynutil.ApplyWithMapper(ctx, r.dynClient, mapper, obj)
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
		if err := dynutil.UpdateStatus(ctx, r.dynClient, mapper, target, sr.Status); err != nil {
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

func (r *collectionReconciler) loadCollection(rc *v1alpha1.ResourceCollection) (*collection.Collection, error) {
	if len(rc.Spec.Inline) > 0 {
		contents := make([]string, len(rc.Spec.Inline))
		for i, t := range rc.Spec.Inline {
			contents[i] = t.Content
		}
		return collection.LoadInline(rc.Name, contents, r.funcMap)
	}

	var userFS fs.FS
	if r.collectionsDir != "" {
		userFS = os.DirFS(r.collectionsDir)
	}
	return collection.Load(rc.Spec.Source, userFS, r.funcMap)
}

func (r *collectionReconciler) handleDeletion(ctx context.Context, rc *v1alpha1.ResourceCollection) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rc, collectionFinalizer) {
		return ctrl.Result{}, nil
	}

	requested, err := r.isRequested(ctx, rc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requested {
		r.log.Info("collection still requested by DeploymentPlan, blocking deletion", "name", rc.Name)
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(rc, collectionFinalizer)
	if err := r.client.Update(ctx, rc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *collectionReconciler) isRequested(ctx context.Context, name string) (bool, error) {
	var planList v1alpha1.DeploymentPlanList
	if err := r.client.List(ctx, &planList); err != nil {
		return false, fmt.Errorf("listing DeploymentPlan: %w", err)
	}
	for _, plan := range planList.Items {
		for i, stepStatus := range plan.Status.Steps {
			if stepStatus.Phase != v1alpha1.StepRequested {
				continue
			}
			if i < len(plan.Spec.Steps) && plan.Spec.Steps[i].Collection != nil && plan.Spec.Steps[i].Collection.Source == name {
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *collectionReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, clusterName string) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		Named("resourcecollection-"+clusterName).
		For(&v1alpha1.ResourceCollection{}).
		Watches(&v1alpha1.DeploymentPlan{}, handler.EnqueueRequestsFromMapFunc(r.planToCollections))

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

		ch, err := r.startFSWatcher(ctx)
		if err != nil {
			r.log.Warn("filesystem watcher not available, changes on disk won't trigger reconcile", "error", err)
		} else {
			builder = builder.WatchesRawSource(source.Channel(ch, &handler.EnqueueRequestForObject{}))
		}
	}

	return builder.Complete(r)
}

func (r *collectionReconciler) planToCollections(ctx context.Context, obj client.Object) []ctrl.Request {
	plan, ok := obj.(*v1alpha1.DeploymentPlan)
	if !ok {
		return nil
	}
	var requests []ctrl.Request
	for i, stepStatus := range plan.Status.Steps {
		if stepStatus.Phase != v1alpha1.StepRequested {
			continue
		}
		if i < len(plan.Spec.Steps) && plan.Spec.Steps[i].Collection != nil {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: plan.Spec.Steps[i].Collection.Source}})
		}
	}
	return requests
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
		if !hasTemplates(filepath.Join(r.collectionsDir, entry.Name())) {
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

func hasTemplates(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext == ".yaml" || ext == ".yml" {
			return true
		}
	}
	return false
}

func (r *collectionReconciler) startFSWatcher(ctx context.Context) (<-chan event.GenericEvent, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(r.collectionsDir); err != nil {
		if closeErr := watcher.Close(); closeErr != nil {
			r.log.Warn("failed to close watcher", "error", closeErr)
		}
		return nil, err
	}

	ch := make(chan event.GenericEvent)

	go func() {
		defer func() {
			if err := watcher.Close(); err != nil {
				r.log.Warn("failed to close watcher", "error", err)
			}
		}()
		defer close(ch)
		for {
			select {
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				r.handleFSEvent(ctx, ev, ch)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				r.log.Warn("fsnotify error", "error", err)
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

func (r *collectionReconciler) handleFSEvent(ctx context.Context, ev fsnotify.Event, ch chan<- event.GenericEvent) {
	rel, err := filepath.Rel(r.collectionsDir, ev.Name)
	if err != nil {
		return
	}
	name := strings.SplitN(rel, string(filepath.Separator), 2)[0]

	if ev.Op&fsnotify.Create != 0 {
		dir := filepath.Join(r.collectionsDir, name)
		if hasTemplates(dir) {
			fsys := os.DirFS(dir)
			if err := r.ensureResourceCollection(ctx, name, fsys); err != nil {
				r.log.Warn("failed to auto-create ResourceCollection from fs event", "name", name, "error", err)
			}
		}
		return
	}

	var rc v1alpha1.ResourceCollection
	if err := r.client.Get(ctx, client.ObjectKey{Name: name}, &rc); err != nil {
		return
	}
	if rc.Spec.Source != "" {
		ch <- event.GenericEvent{Object: &rc}
	}
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
