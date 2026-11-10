package goja

import "github.com/dop251/goja"

type kubeModule struct{}

func (k *kubeModule) Name() string { return "kube" }

func (k *kubeModule) Instantiate(vm *goja.Runtime) goja.Value {
	panic(vm.NewTypeError("kube module is not available yet"))
}
