package goja

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
)

type moduleLoader struct {
	bundleDir string
	vm        *goja.Runtime
	cache     map[string]*goja.Object
	natives   *nativeRegistry
}

func newModuleLoader(vm *goja.Runtime, bundleDir string) *moduleLoader {
	return &moduleLoader{
		bundleDir: bundleDir,
		vm:        vm,
		cache:     make(map[string]*goja.Object),
		natives:   newNativeRegistry(&kubeModule{}),
	}
}

func (ml *moduleLoader) requireFrom(callerDir string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		specifier := call.Argument(0).String()

		if native, ok := ml.natives.lookup(specifier); ok {
			return native.Instantiate(ml.vm)
		}

		if blockedModules.Has(specifier) {
			panic(ml.vm.NewTypeError(fmt.Sprintf("module %q is not available in the Kit agent runtime", specifier)))
		}

		resolved, err := ml.resolve(callerDir, specifier)
		if err != nil {
			panic(ml.vm.NewTypeError(err.Error()))
		}

		if cached, ok := ml.cache[resolved]; ok {
			return cached.Get("exports")
		}

		return ml.load(resolved)
	}
}

func (ml *moduleLoader) resolve(callerDir, specifier string) (string, error) {
	var base string
	if strings.HasPrefix(specifier, "./") || strings.HasPrefix(specifier, "../") {
		base = filepath.Join(callerDir, specifier)
	} else {
		base = filepath.Join(ml.bundleDir, specifier)
	}

	candidates := []string{base, base + ".js", filepath.Join(base, "index.js")}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(abs, filepath.Clean(ml.bundleDir)+string(os.PathSeparator)) {
			return "", fmt.Errorf("module %q resolves outside bundle directory", specifier)
		}
		info, err := os.Stat(abs)
		if err == nil && !info.IsDir() {
			return abs, nil
		}
	}
	return "", fmt.Errorf("cannot find module %q", specifier)
}

func (ml *moduleLoader) load(resolved string) goja.Value {
	source, err := os.ReadFile(resolved)
	if err != nil {
		panic(ml.vm.NewTypeError(fmt.Sprintf("reading module %s: %v", resolved, err)))
	}

	module := ml.vm.NewObject()
	exports := ml.vm.NewObject()
	module.Set("exports", exports)
	ml.cache[resolved] = module

	wrapped := "(function(exports, require, module) {\n" + string(source) + "\n})"
	compiled, err := ml.vm.RunString(wrapped)
	if err != nil {
		delete(ml.cache, resolved)
		panic(ml.vm.NewTypeError(fmt.Sprintf("compiling module %s: %v", resolved, err)))
	}

	fn, ok := goja.AssertFunction(compiled)
	if !ok {
		delete(ml.cache, resolved)
		panic(ml.vm.NewTypeError(fmt.Sprintf("module wrapper is not a function: %s", resolved)))
	}

	moduleDir := filepath.Dir(resolved)
	childRequire := ml.requireFrom(moduleDir)

	_, err = fn(goja.Undefined(), exports, ml.vm.ToValue(childRequire), module)
	if err != nil {
		delete(ml.cache, resolved)
		panic(err)
	}

	return module.Get("exports")
}
