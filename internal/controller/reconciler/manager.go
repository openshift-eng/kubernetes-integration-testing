package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/api/manifests"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	crdv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

var scheme = runtime.NewScheme()

func init() {
	_ = v1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
}

func installManifests(ctx context.Context, log *slog.Logger, cfg *rest.Config) error {
	entries, err := manifests.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("reading embedded manifests: %w", err)
	}

	type manifest struct {
		name string
		raw  []byte
		kind string
	}

	var all []manifest
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := manifests.FS.ReadFile(entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("parsing kind from %s: %w", entry.Name(), err)
		}
		all = append(all, manifest{name: entry.Name(), raw: raw, kind: meta.Kind})
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].name < all[j].name
	})

	tierOrder := func(kind string) int {
		switch kind {
		case "CustomResourceDefinition":
			return 0
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			return 2
		default:
			return 1
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		return tierOrder(all[i].kind) < tierOrder(all[j].kind)
	})

	extClient, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating apiextensions client: %w", err)
	}
	crdClient := extClient.ApiextensionsV1().CustomResourceDefinitions()

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating discovery client: %w", err)
	}

	var crdNames []string
	var mapper meta.RESTMapper
	lastTier := -1

	for _, m := range all {
		tier := tierOrder(m.kind)

		if tier != lastTier && lastTier == 0 {
			for _, name := range crdNames {
				if err := waitForCRD(ctx, crdClient, name); err != nil {
					return err
				}
			}
			groupResources, err := restmapper.GetAPIGroupResources(disc)
			if err != nil {
				return fmt.Errorf("discovering API resources: %w", err)
			}
			mapper = restmapper.NewDiscoveryRESTMapper(groupResources)
		}
		lastTier = tier

		switch m.kind {
		case "CustomResourceDefinition":
			name, err := applyCRD(ctx, log, crdClient, m.raw, m.name)
			if err != nil {
				return err
			}
			crdNames = append(crdNames, name)
		default:
			if err := applyUnstructured(ctx, log, dynClient, mapper, m.raw, m.name); err != nil {
				return err
			}
		}
	}

	if lastTier == 0 {
		for _, name := range crdNames {
			if err := waitForCRD(ctx, crdClient, name); err != nil {
				return err
			}
		}
	}

	return nil
}

func applyCRD(ctx context.Context, log *slog.Logger, crdClient crdv1client.CustomResourceDefinitionInterface, raw []byte, filename string) (string, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return "", fmt.Errorf("parsing %s: %w", filename, err)
	}

	existing, err := crdClient.Get(ctx, crd.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := crdClient.Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("creating CRD %s: %w", crd.Name, err)
		}
		log.Info("installed CRD", "name", crd.Name)
	} else if err != nil {
		return "", fmt.Errorf("getting CRD %s: %w", crd.Name, err)
	} else {
		crd.ResourceVersion = existing.ResourceVersion
		if _, err := crdClient.Update(ctx, &crd, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("updating CRD %s: %w", crd.Name, err)
		}
		log.Info("updated CRD", "name", crd.Name)
	}

	return crd.Name, nil
}

func waitForCRD(ctx context.Context, crdClient crdv1client.CustomResourceDefinitionInterface, name string) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		got, err := crdClient.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, c := range got.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

func applyUnstructured(ctx context.Context, log *slog.Logger, dynClient dynamic.Interface, mapper meta.RESTMapper, raw []byte, filename string) error {
	var obj unstructured.Unstructured
	if err := yaml.Unmarshal(raw, &obj.Object); err != nil {
		return fmt.Errorf("parsing %s: %w", filename, err)
	}

	gvk := obj.GroupVersionKind()
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("mapping %s: %w", gvk, err)
	}

	existing, err := dynClient.Resource(mapping.Resource).Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := dynClient.Resource(mapping.Resource).Create(ctx, &obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		log.Info("installed manifest", "kind", obj.GetKind(), "name", obj.GetName())
	} else if err != nil {
		return fmt.Errorf("getting %s %s: %w", obj.GetKind(), obj.GetName(), err)
	} else {
		obj.SetResourceVersion(existing.GetResourceVersion())
		if _, err := dynClient.Resource(mapping.Resource).Update(ctx, &obj, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		log.Info("updated manifest", "kind", obj.GetKind(), "name", obj.GetName())
	}

	return nil
}
