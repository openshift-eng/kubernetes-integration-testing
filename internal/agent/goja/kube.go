package goja

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dop251/goja"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type kubeModule struct {
	ctx  context.Context
	host agent.Host
}

func (k *kubeModule) Name() string { return "kube" }

func (k *kubeModule) Instantiate(vm *goja.Runtime) goja.Value {
	kc := k.host.Kube()
	obj := vm.NewObject()
	obj.Set("get", k.getFunc(vm, kc))
	obj.Set("list", k.listFunc(vm, kc))
	obj.Set("create", k.createFunc(vm, kc))
	obj.Set("replace", k.replaceFunc(vm, kc))
	obj.Set("patch", k.patchFunc(vm, kc))
	obj.Set("apply", k.applyFunc(vm, kc))
	obj.Set("delete", k.deleteFunc(vm, kc))
	obj.Set("resolveGVR", k.resolveGVRFunc(vm, kc))
	return obj
}

func (k *kubeModule) getFunc(vm *goja.Runtime, kc agent.KubeClient) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		ref := exportRef(vm, call.Argument(0))
		result, err := kc.Get(k.ctx, ref.gvr, ref.namespace, ref.name)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(result.Object)
	}
}

func (k *kubeModule) listFunc(vm *goja.Runtime, kc agent.KubeClient) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		ref := exportRef(vm, call.Argument(0))
		opts := metav1.ListOptions{}
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) {
			optsMap := exportMap(vm, call.Argument(1))
			if ls, ok := optsMap["labelSelector"].(string); ok {
				opts.LabelSelector = ls
			}
			if fs, ok := optsMap["fieldSelector"].(string); ok {
				opts.FieldSelector = fs
			}
		}
		result, err := kc.List(k.ctx, ref.gvr, ref.namespace, opts)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		items := make([]interface{}, len(result.Items))
		for i := range result.Items {
			items[i] = result.Items[i].Object
		}
		return vm.ToValue(items)
	}
}

func (k *kubeModule) createFunc(vm *goja.Runtime, kc agent.KubeClient) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		obj := toUnstructured(vm, call.Argument(0))
		gvr := gvrFromObj(vm, kc, obj)
		result, err := kc.Create(k.ctx, gvr, obj)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(result.Object)
	}
}

func (k *kubeModule) replaceFunc(vm *goja.Runtime, kc agent.KubeClient) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		obj := toUnstructured(vm, call.Argument(0))
		gvr := gvrFromObj(vm, kc, obj)
		result, err := kc.Update(k.ctx, gvr, obj)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(result.Object)
	}
}

func (k *kubeModule) patchFunc(vm *goja.Runtime, kc agent.KubeClient) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		ref := exportRef(vm, call.Argument(0))
		patchObj := call.Argument(1)
		patchData, err := json.Marshal(patchObj.Export())
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("marshaling patch: %w", err)))
		}
		pt := types.MergePatchType
		subresource := ""
		if len(call.Arguments) > 2 && !goja.IsUndefined(call.Argument(2)) {
			optsMap := exportMap(vm, call.Argument(2))
			if sr, ok := optsMap["subresource"].(string); ok {
				subresource = sr
			}
			if tp, ok := optsMap["type"].(string); ok {
				resolved, err := parsePatchType(tp)
				if err != nil {
					panic(vm.NewTypeError(err.Error()))
				}
				pt = resolved
			}
		}
		result, err := kc.Patch(k.ctx, ref.gvr, ref.namespace, ref.name, pt, patchData, subresource)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(result.Object)
	}
}

func (k *kubeModule) applyFunc(vm *goja.Runtime, kc agent.KubeClient) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		obj := toUnstructured(vm, call.Argument(0))
		gvr := gvrFromObj(vm, kc, obj)
		fieldManager := "kit-agent"
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) {
			optsMap := exportMap(vm, call.Argument(1))
			if fm, ok := optsMap["fieldManager"].(string); ok {
				fieldManager = fm
			}
		}
		result, err := kc.Apply(k.ctx, gvr, obj, fieldManager)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(result.Object)
	}
}

func (k *kubeModule) deleteFunc(vm *goja.Runtime, kc agent.KubeClient) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		ref := exportRef(vm, call.Argument(0))
		if err := kc.Delete(k.ctx, ref.gvr, ref.namespace, ref.name); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	}
}

func (k *kubeModule) resolveGVRFunc(vm *goja.Runtime, kc agent.KubeClient) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		m := exportMap(vm, call.Argument(0))
		gvk := schema.GroupVersionKind{
			Group:   stringField(m, "group"),
			Version: stringField(m, "version"),
			Kind:    stringField(m, "kind"),
		}
		gvr, err := kc.ResolveGVR(gvk)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(map[string]string{
			"group":    gvr.Group,
			"version":  gvr.Version,
			"resource": gvr.Resource,
		})
	}
}

type resourceRef struct {
	gvr       schema.GroupVersionResource
	namespace string
	name      string
}

func exportRef(vm *goja.Runtime, val goja.Value) resourceRef {
	m := exportMap(vm, val)
	return resourceRef{
		gvr: schema.GroupVersionResource{
			Group:    stringField(m, "group"),
			Version:  stringField(m, "version"),
			Resource: stringField(m, "resource"),
		},
		namespace: stringField(m, "namespace"),
		name:      stringField(m, "name"),
	}
}

func exportMap(vm *goja.Runtime, val goja.Value) map[string]interface{} {
	exported := val.Export()
	m, ok := exported.(map[string]interface{})
	if !ok {
		panic(vm.NewTypeError(fmt.Sprintf("expected object, got %T", exported)))
	}
	return m
}

func stringField(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func toUnstructured(vm *goja.Runtime, val goja.Value) *unstructured.Unstructured {
	m := exportMap(vm, val)
	return &unstructured.Unstructured{Object: m}
}

func parsePatchType(s string) (types.PatchType, error) {
	switch s {
	case "merge":
		return types.MergePatchType, nil
	case "strategic":
		return types.StrategicMergePatchType, nil
	case "json":
		return types.JSONPatchType, nil
	default:
		return "", fmt.Errorf("unknown patch type %q: must be merge, strategic, or json", s)
	}
}

func gvrFromObj(vm *goja.Runtime, kc agent.KubeClient, obj *unstructured.Unstructured) schema.GroupVersionResource {
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" {
		panic(vm.NewTypeError("object must have apiVersion and kind"))
	}
	gvr, err := kc.ResolveGVR(gvk)
	if err != nil {
		panic(vm.NewGoError(err))
	}
	return gvr
}
