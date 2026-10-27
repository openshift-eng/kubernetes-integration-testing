package reconciler

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"

	"github.com/fsnotify/fsnotify"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	kubeConfig     *kube.KubeConfig
	collectionsDir string
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
	}

	referenced, err := r.isReferenced(ctx, rc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !referenced {
		return ctrl.Result{}, nil
	}

	renderResult, err := r.render(ctx, &rc)
	if err != nil {
		base := rc.DeepCopy()
		rc.Status.Phase = v1alpha1.ResourceCollectionFailed
		rc.Status.Message = err.Error()
		if uerr := r.client.Status().Patch(ctx, &rc, client.MergeFrom(base)); uerr != nil {
			r.log.Error("updating failed status", "name", rc.Name, "error", uerr)
		}
		return ctrl.Result{}, err
	}

	currentHash := rc.GetAnnotations()[collection.ContentHashAnnotation]
	if rc.Status.Phase == v1alpha1.ResourceCollectionApplied &&
		rc.Status.ObservedGeneration == rc.Generation &&
		currentHash == renderResult.Hash {
		return ctrl.Result{}, nil
	}

	r.log.Info("applying resource collection", "name", rc.Name, "oldHash", currentHash, "newHash", renderResult.Hash)
	if err := r.apply(ctx, &rc, renderResult); err != nil {
		base := rc.DeepCopy()
		rc.Status.Phase = v1alpha1.ResourceCollectionFailed
		rc.Status.Message = err.Error()
		if uerr := r.client.Status().Patch(ctx, &rc, client.MergeFrom(base)); uerr != nil {
			r.log.Error("updating failed status", "name", rc.Name, "error", uerr)
		}
		return ctrl.Result{}, err
	}

	annotations := rc.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[collection.ContentHashAnnotation] = renderResult.Hash
	rc.SetAnnotations(annotations)
	if err := r.client.Update(ctx, &rc); err != nil {
		return ctrl.Result{}, err
	}

	base := rc.DeepCopy()
	rc.Status.Phase = v1alpha1.ResourceCollectionApplied
	rc.Status.Message = ""
	rc.Status.ObservedGeneration = rc.Generation
	if err := r.client.Status().Patch(ctx, &rc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	r.log.Info("applied resource collection", "name", rc.Name)
	return ctrl.Result{}, nil
}

func (r *collectionReconciler) render(ctx context.Context, rc *v1alpha1.ResourceCollection) (*collection.RenderResult, error) {
	coll, err := r.loadCollection(rc)
	if err != nil {
		return nil, err
	}

	var kcList v1alpha1.KitClusterList
	if err := r.client.List(ctx, &kcList); err != nil {
		return nil, fmt.Errorf("listing KitCluster: %w", err)
	}
	if len(kcList.Items) == 0 {
		return nil, fmt.Errorf("no KitCluster found")
	}
	kc := kcList.Items[0]

	// TODO: Platform and NetworkType should come from KitCluster spec.
	renderCtx := collection.RenderContext{
		FeatureSet:   kc.Spec.FeatureSet,
		APIServerURL: kc.Status.APIServerURL,
		Platform:     "None",
		NetworkType:  "OVNKubernetes",
		Values:       rc.Spec.Values,
	}

	return coll.Render(renderCtx)
}

func (r *collectionReconciler) apply(ctx context.Context, rc *v1alpha1.ResourceCollection, result *collection.RenderResult) error {
	cfg, err := r.kubeConfig.RestConfig()
	if err != nil {
		return fmt.Errorf("building rest config: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating discovery client: %w", err)
	}

	groupResources, err := restmapper.GetAPIGroupResources(disc)
	if err != nil {
		return fmt.Errorf("discovering API resources: %w", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	applier := collection.NewApplier(dynClient, mapper, r.log)
	return applier.ApplyRendered(ctx, result)
}

func (r *collectionReconciler) loadCollection(rc *v1alpha1.ResourceCollection) (*collection.Collection, error) {
	if len(rc.Spec.Inline) > 0 {
		contents := make([]string, len(rc.Spec.Inline))
		for i, t := range rc.Spec.Inline {
			contents[i] = t.Content
		}
		return collection.LoadInline(rc.Name, contents)
	}

	var userFS fs.FS
	if r.collectionsDir != "" {
		userFS = os.DirFS(r.collectionsDir)
	}
	return collection.Load(rc.Spec.Source, userFS)
}

func (r *collectionReconciler) handleDeletion(ctx context.Context, rc *v1alpha1.ResourceCollection) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rc, collectionFinalizer) {
		return ctrl.Result{}, nil
	}

	referenced, err := r.isReferenced(ctx, rc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if referenced {
		r.log.Info("collection still referenced by KitCluster, blocking deletion", "name", rc.Name)
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(rc, collectionFinalizer)
	if err := r.client.Update(ctx, rc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *collectionReconciler) isReferenced(ctx context.Context, name string) (bool, error) {
	var kcList v1alpha1.KitClusterList
	if err := r.client.List(ctx, &kcList); err != nil {
		return false, fmt.Errorf("listing KitCluster: %w", err)
	}
	for _, kc := range kcList.Items {
		for _, ref := range kc.Spec.Collections {
			if ref == name {
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
		Watches(&v1alpha1.KitCluster{}, handler.EnqueueRequestsFromMapFunc(r.kitClusterToCollections))

	for _, name := range collection.BuiltinNames() {
		if err := r.ensureResourceCollection(ctx, name); err != nil {
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

func (r *collectionReconciler) kitClusterToCollections(ctx context.Context, obj client.Object) []ctrl.Request {
	kc, ok := obj.(*v1alpha1.KitCluster)
	if !ok {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(kc.Spec.Collections))
	for _, name := range kc.Spec.Collections {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
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
		if err := r.ensureResourceCollection(ctx, entry.Name()); err != nil {
			r.log.Warn("failed to auto-create ResourceCollection", "name", entry.Name(), "error", err)
		}
	}
	return nil
}

func (r *collectionReconciler) ensureResourceCollection(ctx context.Context, name string) error {
	rc := &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ResourceCollectionSpec{Source: name},
	}
	if err := r.client.Create(ctx, rc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	r.log.Info("auto-created ResourceCollection from disk", "name", name)
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
		watcher.Close()
		return nil, err
	}

	ch := make(chan event.GenericEvent)

	go func() {
		defer watcher.Close()
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
			if err := r.ensureResourceCollection(ctx, name); err != nil {
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
