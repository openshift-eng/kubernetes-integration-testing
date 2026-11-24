package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/dynutil"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
)

const (
	kindKitCluster = "KitCluster"
	kindCRD        = "CustomResourceDefinition"

	// defaultResourceTimeout bounds the resource apply and wait phase
	// when the request does not set one.
	defaultResourceTimeout = 10 * time.Minute

	// resourceFieldManager is the server-side apply field manager the
	// daemon applies user resources with.
	resourceFieldManager = "kit-daemon"
)

// waitConditionFor returns the status condition that signals readiness
// for the kit kinds that have one. Kit kinds without a readiness
// condition are considered ready as soon as they are applied.
func waitConditionFor(obj *unstructured.Unstructured) (string, bool) {
	switch obj.GetKind() {
	case "DeploymentPlan":
		return v1alpha1.ConditionPlanReady, true
	case "ResourceCollection":
		return v1alpha1.ConditionCollectionReady, true
	}
	return "", false
}

// waitSettled reports whether an Unknown condition means nothing more
// will happen without new input: a collection nothing references is never
// evaluated (rendering has side effects), so its Ready condition stays
// Unknown forever — waiting on it would only burn the timeout. Extra
// collections in a manifest must not block the deploy.
func waitSettled(obj *unstructured.Unstructured, cond *metav1.Condition) bool {
	return obj.GetKind() == "ResourceCollection" &&
		cond.Status == metav1.ConditionUnknown &&
		cond.Reason == v1alpha1.ReasonCollectionNotReferenced
}

// prepareResources parses and validates resource manifests ahead of
// cluster creation, so that invalid input fails before any cluster
// exists. Only kit CRs are accepted: kit CRDs are installed by the
// controller during bootstrap and payload CRDs come from the release
// image, so CRDs are never passed as resources.
func prepareResources(resources [][]byte) ([]unstructured.Unstructured, error) {
	objs := make([]unstructured.Unstructured, 0, len(resources))
	for i, raw := range resources {
		var obj unstructured.Unstructured
		if err := yaml.Unmarshal(raw, &obj.Object); err != nil {
			return nil, fmt.Errorf("parsing resource %d: %w", i, err)
		}
		switch {
		case obj.GetAPIVersion() == "":
			return nil, fmt.Errorf("resource %d is missing apiVersion", i)
		case obj.GetKind() == "":
			return nil, fmt.Errorf("resource %d is missing kind", i)
		case obj.GetName() == "":
			return nil, fmt.Errorf("resource %d is missing metadata.name", i)
		case obj.GetKind() == kindKitCluster:
			return nil, fmt.Errorf("the KitCluster is created from the cluster spec itself and cannot be passed as a resource")
		case obj.GetKind() == kindCRD:
			return nil, fmt.Errorf("CRDs cannot be passed as resources: kit CRDs are installed by the controller at startup and payload CRDs come from the release image")
		case obj.GroupVersionKind().Group != v1alpha1.GroupVersion.Group:
			return nil, fmt.Errorf("resource %d (%s %s) is not a kit CR: only kit CRs can be passed as resources", i, obj.GetKind(), obj.GetName())
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

// applyResources applies kit CRs to the cluster behind kubeconfigPath, in
// order, and waits for readiness of the kinds that expose a readiness
// condition. The whole phase is bounded by timeout.
//
// By the time the daemon gets here the controller is healthy, so the kit
// CRDs are established and the API is served; the apply is a single
// attempt that fails fast on anything unexpected. Diagnostics travel in
// the returned error, which names the resource and the last observed
// condition message.
//
// On failure the cluster is left running for debugging; the caller is
// responsible for destroying it.
func applyResources(ctx context.Context, log *slog.Logger, kubeconfigPath string, objs []unstructured.Unstructured, wait bool, timeout time.Duration) error {
	clusterKC, err := kube.New(kubeconfigPath, "")
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	cfg, err := clusterKC.RestConfig()
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
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

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for i := range objs {
		obj := &objs[i]

		if _, err := dynutil.ApplyWithMapperAs(ctx, dynClient, mapper, obj, resourceFieldManager); err != nil {
			return applyError(obj, err)
		}
		log.Info("applied resource", "kind", obj.GetKind(), "name", obj.GetName())
	}

	if !wait {
		return nil
	}

	for i := range objs {
		obj := &objs[i]
		condition, waitable := waitConditionFor(obj)
		if !waitable {
			continue
		}

		if err := pollReady(ctx, log, dynClient, mapper, obj, condition); err != nil {
			return err
		}
		log.Info("resource ready", "kind", obj.GetKind(), "name", obj.GetName())
	}

	return nil
}

// applyError renders an apply failure, adding guidance when the kind is
// not served by the cluster.
func applyError(obj *unstructured.Unstructured, err error) error {
	var noMatch *meta.NoKindMatchError
	if errors.As(err, &noMatch) {
		return fmt.Errorf("applying %s %s: %s is not served by the cluster (unknown or wrong apiVersion; kit CRDs are established before the cluster is reported ready)", obj.GetKind(), obj.GetName(), obj.GroupVersionKind())
	}
	return fmt.Errorf("applying %s %s: %w", obj.GetKind(), obj.GetName(), err)
}

// pollReady polls the status conditions of obj until the given condition
// is True, with the last observed condition message in the error. An
// Unknown condition that waitSettled considers final ends the wait: the
// resource reached a stable state that will not become ready on its own.
func pollReady(ctx context.Context, log *slog.Logger, dynClient dynamic.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured, condition string) error {
	res, err := dynutil.ResourceClient(dynClient, mapper, obj)
	if err != nil {
		return err
	}

	var lastMessage string
	err = wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		got, err := res.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			// The object was just applied; transient failures are
			// retried until the timeout.
			if !apierrors.IsNotFound(err) {
				lastMessage = err.Error()
			}
			return false, nil
		}
		cond := meta.FindStatusCondition(statusConditions(got), condition)
		if cond == nil {
			return false, nil
		}
		lastMessage = cond.Message
		if cond.Status == metav1.ConditionTrue {
			return true, nil
		}
		if waitSettled(obj, cond) {
			log.Info("condition settled without becoming ready, not waiting",
				"kind", obj.GetKind(), "name", obj.GetName(), "reason", cond.Reason, "message", cond.Message)
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		if lastMessage != "" {
			return fmt.Errorf("waiting for %s %s condition %s: %w (last observed: %s)", obj.GetKind(), obj.GetName(), condition, err, lastMessage)
		}
		return fmt.Errorf("waiting for %s %s condition %s: %w", obj.GetKind(), obj.GetName(), condition, err)
	}
	return nil
}

// statusConditions extracts status.conditions as []metav1.Condition. It
// returns nil when the object has no conditions.
func statusConditions(obj *unstructured.Unstructured) []metav1.Condition {
	raw, ok, err := unstructured.NestedFieldNoCopy(obj.Object, "status", "conditions")
	if !ok || err != nil || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var conds []metav1.Condition
	if err := json.Unmarshal(data, &conds); err != nil {
		return nil
	}
	return conds
}
