package kube

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

// Client implements agent.KubeClient using the dynamic client.
type Client struct {
	dynamic dynamic.Interface
	mapper  meta.RESTMapper
	policy  *Policy
}

func NewClient(dyn dynamic.Interface, mapper meta.RESTMapper, policy *Policy) *Client {
	return &Client{dynamic: dyn, mapper: mapper, policy: policy}
}

func (c *Client) SetCapabilities(targetNamespace string, caps []collection.AtomicCapability) {
	c.policy = NewPolicy(targetNamespace, caps)
}

func (c *Client) checkAccess(gvr schema.GroupVersionResource, verb, namespace, subresource string) error {
	if c.policy == nil {
		return fmt.Errorf("missing capability: %s %s/%s (agent policy not initialized)", verb, gvr.Group, gvr.Resource)
	}
	return c.policy.CheckAccess(AccessRequest{
		GVR:         gvr,
		Verb:        verb,
		Namespace:   namespace,
		Subresource: subresource,
	})
}

func (c *Client) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	if err := c.checkAccess(gvr, "get", namespace, ""); err != nil {
		return nil, err
	}
	return c.resource(gvr, namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) List(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if err := c.checkAccess(gvr, "list", namespace, ""); err != nil {
		return nil, err
	}
	return c.resource(gvr, namespace).List(ctx, opts)
}

func (c *Client) Create(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ns := ""
	if obj != nil {
		ns = obj.GetNamespace()
	}
	if err := c.checkAccess(gvr, "create", ns, ""); err != nil {
		return nil, err
	}
	return c.resource(gvr, ns).Create(ctx, obj, metav1.CreateOptions{})
}

func (c *Client) Update(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ns := ""
	if obj != nil {
		ns = obj.GetNamespace()
	}
	if err := c.checkAccess(gvr, "update", ns, ""); err != nil {
		return nil, err
	}
	return c.resource(gvr, ns).Update(ctx, obj, metav1.UpdateOptions{})
}

func (c *Client) Patch(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, pt types.PatchType, data []byte, subresource string) (*unstructured.Unstructured, error) {
	if err := c.checkAccess(gvr, "patch", namespace, subresource); err != nil {
		return nil, err
	}
	opts := metav1.PatchOptions{}
	if subresource != "" {
		return c.resource(gvr, namespace).Patch(ctx, name, pt, data, opts, subresource)
	}
	return c.resource(gvr, namespace).Patch(ctx, name, pt, data, opts)
}

func (c *Client) Apply(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, fieldManager string) (*unstructured.Unstructured, error) {
	ns := ""
	if obj != nil {
		ns = obj.GetNamespace()
	}
	if err := c.checkAccess(gvr, "patch", ns, ""); err != nil {
		return nil, err
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshaling object for SSA: %w", err)
	}
	return c.resource(gvr, obj.GetNamespace()).Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		FieldManager: fieldManager,
		Force:        ptr.To(true),
	})
}

func (c *Client) Delete(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	if err := c.checkAccess(gvr, "delete", namespace, ""); err != nil {
		return err
	}
	return c.resource(gvr, namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *Client) Watch(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (watch.Interface, error) {
	if err := c.checkAccess(gvr, "watch", namespace, ""); err != nil {
		return nil, err
	}
	return c.resource(gvr, namespace).Watch(ctx, opts)
}

func (c *Client) ResolveGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("resolving %s: %w", gvk.String(), err)
	}
	return mapping.Resource, nil
}

func (c *Client) resource(gvr schema.GroupVersionResource, namespace string) dynamic.ResourceInterface {
	if namespace != "" {
		return c.dynamic.Resource(gvr).Namespace(namespace)
	}
	return c.dynamic.Resource(gvr)
}
