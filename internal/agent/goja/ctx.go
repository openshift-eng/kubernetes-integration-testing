package goja

import (
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/dop251/goja"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
)

type exitSignal struct {
	code   int
	reason string
}

func newCtxObject(active *ctxHolder, vm *goja.Runtime, req agent.AgentRequest, host agent.Host, loop *agent.EventLoop, log *slog.Logger, shutdownGrace time.Duration, shutdown *agent.ShutdownHandler) goja.Value {
	obj := vm.NewObject()

	obj.Set("log", func(call goja.FunctionCall) goja.Value {
		host.Log(call.Argument(0).String())
		return goja.Undefined()
	})

	obj.Set("sleep", func(call goja.FunctionCall) goja.Value {
		ms := call.Argument(0).ToInteger()
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-active.ctx.Done():
		}
		return goja.Undefined()
	})

	obj.Set("values", vm.ToValue(req.Values))

	pod := vm.NewObject()
	pod.Set("name", req.Pod.Name)
	pod.Set("namespace", req.Pod.Namespace)
	pod.Set("uid", string(req.Pod.UID))
	pod.Set("nodeName", req.Pod.NodeName)
	obj.Set("pod", pod)

	obj.Set("env", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		if !isAllowedEnv(name) {
			return goja.Undefined()
		}
		return vm.ToValue(os.Getenv(name))
	})

	obj.Set("exit", func(call goja.FunctionCall) goja.Value {
		code := int(call.Argument(0).ToInteger())
		reason := ""
		if len(call.Arguments) > 1 {
			reason = call.Argument(1).String()
		}
		panic(exitSignal{code: code, reason: reason})
	})

	obj.Set("run", func(call goja.FunctionCall) goja.Value {
		rr := loop.Run()
		switch rr.Outcome {
		case agent.OutcomeCanceled, agent.OutcomeWatchError:
			panic(exitSignal{code: rr.Result.ExitCode, reason: rr.Result.Reason})
		case agent.OutcomeNone:
			return goja.Undefined()
		case agent.OutcomeTerminal:
			ret := vm.NewObject()
			ret.Set("exitCode", rr.Result.ExitCode)
			ret.Set("reason", rr.Result.Reason)
			return ret
		}
		return goja.Undefined()
	})

	obj.Set("onShutdown", func(call goja.FunctionCall) goja.Value {
		cb, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			panic(vm.NewTypeError("onShutdown callback must be a function"))
		}
		*shutdown = &gojaShutdownHandler{vm: vm, cb: cb, log: log, gracePeriod: shutdownGrace}
		return goja.Undefined()
	})

	return obj
}

func isAllowedEnv(name string) bool {
	if strings.HasPrefix(name, "KIT_") {
		return true
	}
	switch name {
	case "KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT",
		"POD_NAME", "POD_NAMESPACE", "NODE_NAME":
		return true
	}
	return false
}
