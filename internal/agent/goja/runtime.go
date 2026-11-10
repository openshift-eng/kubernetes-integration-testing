package goja

import (
	"context"
	"errors"
	"fmt"

	"github.com/dop251/goja"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
)

type gojaRuntime struct{}

func New() agent.Runtime {
	return &gojaRuntime{}
}

func (r *gojaRuntime) Name() string { return "js" }

func (r *gojaRuntime) Run(ctx context.Context, req agent.AgentRequest, host agent.Host) (result agent.AgentResult, err error) {
	vm := goja.New()

	loader := newModuleLoader(vm, req.BundleDir)
	vm.Set("require", loader.requireFrom(req.BundleDir))

	ctxObj := newCtxObject(ctx, vm, req, host)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-done:
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			if sig, ok := r.(exitSignal); ok {
				result = agent.AgentResult{ExitCode: sig.code, Reason: sig.reason}
				err = nil
				return
			}
			if e, ok := r.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("unexpected panic: %v", r)
		}
	}()

	entrypoint := "./" + req.Entrypoint
	resolved, resolveErr := loader.resolve(req.BundleDir, entrypoint)
	if resolveErr != nil {
		return agent.AgentResult{}, fmt.Errorf("resolving entrypoint %q: %w", req.Entrypoint, resolveErr)
	}
	exports := loader.load(resolved)

	mainFn, err := findMain(vm, exports)
	if err != nil {
		return agent.AgentResult{}, err
	}

	retVal, err := mainFn(goja.Undefined(), ctxObj)
	if err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			return agent.AgentResult{}, ctx.Err()
		}
		var exception *goja.Exception
		if errors.As(err, &exception) {
			return agent.AgentResult{ExitCode: 1, Reason: exception.Value().String()}, nil
		}
		return agent.AgentResult{}, fmt.Errorf("executing script: %w", err)
	}

	return parseResult(retVal), nil
}

func findMain(vm *goja.Runtime, exports goja.Value) (goja.Callable, error) {
	if fn, ok := goja.AssertFunction(exports); ok {
		return fn, nil
	}

	obj, ok := exports.(*goja.Object)
	if !ok {
		return nil, fmt.Errorf("entrypoint module exports is not an object")
	}

	mainVal := obj.Get("main")
	if mainVal == nil || goja.IsUndefined(mainVal) {
		return nil, fmt.Errorf("entrypoint module does not export a main function")
	}

	fn, ok := goja.AssertFunction(mainVal)
	if !ok {
		return nil, fmt.Errorf("entrypoint module.main is not a function")
	}
	return fn, nil
}

func parseResult(val goja.Value) agent.AgentResult {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return agent.AgentResult{}
	}

	obj, ok := val.(*goja.Object)
	if !ok {
		return agent.AgentResult{}
	}

	result := agent.AgentResult{}

	if exitCode := obj.Get("exitCode"); exitCode != nil && !goja.IsUndefined(exitCode) {
		result.ExitCode = int(exitCode.ToInteger())
	}

	if reason := obj.Get("reason"); reason != nil && !goja.IsUndefined(reason) {
		result.Reason = reason.String()
	}

	return result
}
