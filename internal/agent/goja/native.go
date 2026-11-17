package goja

import "github.com/dop251/goja"

type NativeModule interface {
	Name() string
	Instantiate(vm *goja.Runtime) goja.Value
}

type nativeRegistry struct {
	modules map[string]NativeModule
}

func newNativeRegistry(modules ...NativeModule) *nativeRegistry {
	r := &nativeRegistry{modules: make(map[string]NativeModule, len(modules))}
	for _, m := range modules {
		r.modules[m.Name()] = m
	}
	return r
}

func (r *nativeRegistry) lookup(name string) (NativeModule, bool) {
	m, ok := r.modules[name]
	return m, ok
}
