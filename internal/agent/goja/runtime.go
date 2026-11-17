package goja

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dop251/goja"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
)

// ctxHolder holds the active context for kube operations and ctx.sleep().
// During normal execution it holds the original context; during shutdown it
// is swapped to a grace-period context so cleanup operations can complete.
// Single-goroutine safe: only accessed from the goja goroutine.
type ctxHolder struct {
	ctx context.Context
}

type gojaRuntime struct {
	shutdownGracePeriod time.Duration
}

func New(shutdownGracePeriod time.Duration) agent.Runtime {
	return &gojaRuntime{shutdownGracePeriod: shutdownGracePeriod}
}

func (r *gojaRuntime) Name() string { return "js" }

func (r *gojaRuntime) Run(ctx context.Context, req agent.AgentRequest, host agent.Host) (agent.AgentResult, error) {
	log := slog.With("pod", req.Pod.Name, "entrypoint", req.Entrypoint)
	loop := agent.NewEventLoop(ctx)
	defer loop.Stop()

	active := &ctxHolder{ctx: ctx}
	var shutdown agent.ShutdownHandler
	candidate, candidateErr := execute(ctx, req, host, loop, log, r.shutdownGracePeriod, active, &shutdown)
	return finalize(ctx, loop, log, r.shutdownGracePeriod, active, shutdown, candidate, candidateErr)
}

// execute runs the script to completion and returns a candidate result. Every
// return path is a candidate — the final decision belongs to finalize.
func execute(ctx context.Context, req agent.AgentRequest, host agent.Host, loop *agent.EventLoop, log *slog.Logger, shutdownGrace time.Duration, active *ctxHolder, shutdown *agent.ShutdownHandler) (result agent.AgentResult, err error) {
	vm := goja.New()

	loader := newModuleLoader(vm, req.BundleDir, active, host, loop, log)
	if err := vm.Set("require", loader.requireFrom(req.BundleDir)); err != nil {
		return agent.AgentResult{}, fmt.Errorf("setting up VM: %w", err)
	}

	ctxObj := newCtxObject(active, vm, req, host, loop, log, shutdownGrace, shutdown)

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
			return agent.AgentResult{}, err
		}
		var exception *goja.Exception
		if errors.As(err, &exception) {
			return agent.AgentResult{ExitCode: 1, Reason: exception.Value().String()}, nil
		}
		return agent.AgentResult{}, fmt.Errorf("executing script: %w", err)
	}

	return parseResult(retVal), nil
}

// finalize is the single linearization point for the agent result. If the
// context was cancelled, cancellation wins over any script-produced candidate.
func finalize(ctx context.Context, loop *agent.EventLoop, log *slog.Logger, gracePeriod time.Duration, active *ctxHolder, shutdown agent.ShutdownHandler, candidate agent.AgentResult, candidateErr error) (agent.AgentResult, error) {
	if ctx.Err() != nil {
		loop.HandleCancellation()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gracePeriod)
		defer cancel()
		active.ctx = shutdownCtx
		runShutdown(log, shutdown)
		return cancelResult(ctx)
	}
	return candidate, candidateErr
}

// runShutdown executes the shutdown handler with its own recover so that
// ctx.exit() or panics inside the callback cannot crash the runtime.
func runShutdown(log *slog.Logger, h agent.ShutdownHandler) {
	if h == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(exitSignal); !ok {
				log.Warn("onShutdown panic", "error", r)
			}
		}
	}()
	h.OnShutdown()
}

// cancelResult implements the cancellation policy: SIGTERM (context.Canceled)
// is a graceful exit; timeout (DeadlineExceeded) is an error.
func cancelResult(ctx context.Context) (agent.AgentResult, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return agent.AgentResult{ExitCode: 0}, nil
	}
	return agent.AgentResult{}, ctx.Err()
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
