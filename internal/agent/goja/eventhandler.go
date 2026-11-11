package goja

import (
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
)

type gojaEventHandler struct {
	vm *goja.Runtime
	cb goja.Callable
}

func (h *gojaEventHandler) OnEvent(event agent.WatchEvent) (agent.AgentResult, bool) {
	obj := h.vm.NewObject()
	obj.Set("type", event.Type)
	obj.Set("object", h.vm.ToValue(event.Object))

	retVal, err := h.cb(goja.Undefined(), obj)
	if err != nil {
		return agent.AgentResult{ExitCode: 1, Reason: err.Error()}, true
	}

	if retVal == nil || goja.IsUndefined(retVal) || goja.IsNull(retVal) {
		return agent.AgentResult{}, false
	}

	retObj, ok := retVal.(*goja.Object)
	if !ok {
		return agent.AgentResult{}, false
	}

	exitCode := retObj.Get("exitCode")
	if exitCode == nil || goja.IsUndefined(exitCode) {
		return agent.AgentResult{}, false
	}

	result := agent.AgentResult{ExitCode: int(exitCode.ToInteger())}
	if reason := retObj.Get("reason"); reason != nil && !goja.IsUndefined(reason) {
		result.Reason = reason.String()
	}
	return result, true
}

type gojaShutdownHandler struct {
	vm          *goja.Runtime
	cb          goja.Callable
	log         *slog.Logger
	gracePeriod time.Duration
}

func (h *gojaShutdownHandler) OnShutdown() {
	h.vm.ClearInterrupt()

	var expired atomic.Bool
	timer := time.AfterFunc(h.gracePeriod, func() {
		expired.Store(true)
		h.vm.Interrupt(fmt.Errorf("shutdown grace period exceeded"))
	})
	defer timer.Stop()

	if _, err := h.cb(goja.Undefined()); err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) && !expired.Load() {
			h.vm.ClearInterrupt()
			if expired.Load() {
				return
			}
			if _, retryErr := h.cb(goja.Undefined()); retryErr != nil {
				h.log.Error("onShutdown callback failed on retry", "error", retryErr)
			}
			return
		}
		h.log.Error("onShutdown callback failed", "error", err)
	}
}
