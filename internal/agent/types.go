package agent

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Runtime interface {
	Name() string
	Run(ctx context.Context, req AgentRequest, host Host) (AgentResult, error)
}

type Host interface {
	Log(msg string)
	Kube() KubeClient
}

type KubeClient interface {
	Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
	List(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Create(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Update(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Patch(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, pt types.PatchType, data []byte, subresource string) (*unstructured.Unstructured, error)
	Apply(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, fieldManager string) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error
	Watch(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (watch.Interface, error)
	ResolveGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error)
}

type AgentRequest struct {
	BundleDir  string
	Entrypoint string
	Values     map[string]any
	Pod        PodReference
}

type AgentResult struct {
	ExitCode int
	Reason   string
}

type PodReference struct {
	Name      string
	Namespace string
	UID       types.UID
	NodeName  string
}

