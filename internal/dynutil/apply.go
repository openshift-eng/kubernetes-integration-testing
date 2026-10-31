package dynutil

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
)

func Apply(ctx context.Context, client dynamic.ResourceInterface, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshaling %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return client.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		FieldManager: "kit-controller",
		Force:        ptr.To(true),
	})
}

func ApplyWithMapper(ctx context.Context, client dynamic.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	res, err := ResourceClient(client, mapper, obj)
	if err != nil {
		return nil, err
	}
	return Apply(ctx, res, obj)
}

func UpdateStatus(ctx context.Context, dynClient dynamic.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured, status map[string]interface{}) error {
	res, err := ResourceClient(dynClient, mapper, obj)
	if err != nil {
		return fmt.Errorf("resolving resource for %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}

	existing, err := res.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}

	if err := unstructured.SetNestedField(existing.Object, status, "status"); err != nil {
		return fmt.Errorf("setting status on %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}

	_, err = res.UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	return err
}

func ResourceClient(client dynamic.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := obj.GroupVersionKind()
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("mapping %s: %w", gvk.String(), err)
	}

	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		return client.Resource(mapping.Resource).Namespace(ns), nil
	}
	return client.Resource(mapping.Resource), nil
}
