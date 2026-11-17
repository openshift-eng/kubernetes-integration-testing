package agent

import (
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
)

type Outcome int

const (
	OutcomeNone       Outcome = iota // no watches registered, script continues
	OutcomeTerminal                  // handler decided to stop
	OutcomeCanceled                  // context cancelled
	OutcomeWatchError                // terminal watch error
)

type RunResult struct {
	Outcome Outcome
	Result  AgentResult
}

type WatchEvent struct {
	Type   string
	Object map[string]interface{}
}

type EventHandler interface {
	OnEvent(event WatchEvent) (AgentResult, bool)
}

type ShutdownHandler interface {
	OnShutdown()
}

type taggedEvent struct {
	handler EventHandler
	event   watch.Event
	closed  bool
}

type inputKind int

const (
	inputCancel inputKind = iota
	inputNone
	inputClosed
	inputEvent
	inputWatchError
)

type loopInput struct {
	kind    inputKind
	handler EventHandler
	event   WatchEvent
	raw     watch.Event
}

// EventLoop manages watch subscriptions and dispatches events to handlers.
// It is not goroutine-safe: all methods must be called from a single goroutine.
// The EventLoop only manages resources; lifecycle decisions (shutdown callbacks,
// result policy) belong to the runtime.
type EventLoop struct {
	ctx      context.Context
	fanIn    chan taggedEvent
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	watchers []watch.Interface
	active   int
}

func NewEventLoop(ctx context.Context) *EventLoop {
	return &EventLoop{
		ctx:    ctx,
		fanIn:  make(chan taggedEvent),
		stopCh: make(chan struct{}),
	}
}

func (el *EventLoop) Stopped() bool {
	select {
	case <-el.stopCh:
		return true
	default:
		return false
	}
}

func (el *EventLoop) AddWatch(w watch.Interface, h EventHandler) error {
	select {
	case <-el.stopCh:
		w.Stop()
		return fmt.Errorf("event loop is stopped")
	default:
	}
	el.watchers = append(el.watchers, w)
	el.active++
	el.wg.Add(1)
	go el.pump(w, h)
	return nil
}

// HandleCancellation stops all watches. Idempotent.
func (el *EventLoop) HandleCancellation() {
	el.Stop()
}

// next is the single input boundary: when active watches exist, it enforces
// cancellation priority over any pending event.
func (el *EventLoop) next() loopInput {
	if el.active == 0 {
		return loopInput{kind: inputNone}
	}
	select {
	case <-el.ctx.Done():
		return loopInput{kind: inputCancel}
	case te := <-el.fanIn:
		if el.ctx.Err() != nil {
			return loopInput{kind: inputCancel}
		}
		if te.closed {
			return loopInput{kind: inputClosed}
		}
		if te.event.Type == watch.Error {
			return loopInput{kind: inputWatchError, raw: te.event}
		}
		return loopInput{kind: inputEvent, handler: te.handler, event: toWatchEvent(te.event)}
	}
}

func (el *EventLoop) Run() RunResult {
	for {
		in := el.next()
		switch in.kind {
		case inputCancel:
			el.HandleCancellation()
			return RunResult{Outcome: OutcomeCanceled}
		case inputNone:
			return RunResult{Outcome: OutcomeNone}
		case inputClosed:
			el.active--
			if el.active == 0 {
				return RunResult{Outcome: OutcomeNone}
			}
		case inputWatchError:
			el.Stop()
			return RunResult{
				Outcome: OutcomeWatchError,
				Result:  AgentResult{ExitCode: 1, Reason: watchErrorReason(in.raw)},
			}
		case inputEvent:
			result, stop := in.handler.OnEvent(in.event)
			if stop {
				el.Stop()
				return RunResult{Outcome: OutcomeTerminal, Result: result}
			}
		}
	}
}

// Stop closes all watches and waits for pump goroutines to exit.
func (el *EventLoop) Stop() {
	el.stopOnce.Do(func() {
		close(el.stopCh)
	})
	for _, w := range el.watchers {
		w.Stop()
	}
	el.watchers = nil
	el.active = 0
	el.wg.Wait()
}

func (el *EventLoop) pump(w watch.Interface, h EventHandler) {
	defer el.wg.Done()
	for ev := range w.ResultChan() {
		select {
		case el.fanIn <- taggedEvent{handler: h, event: ev}:
		case <-el.stopCh:
			return
		case <-el.ctx.Done():
			return
		}
	}
	select {
	case el.fanIn <- taggedEvent{closed: true}:
	case <-el.stopCh:
	case <-el.ctx.Done():
	}
}

func watchErrorReason(ev watch.Event) string {
	if status, ok := ev.Object.(*metav1.Status); ok {
		if status.Message != "" {
			return status.Message
		}
		if status.Reason != "" {
			return string(status.Reason)
		}
		if status.Code != 0 {
			return fmt.Sprintf("HTTP %d", status.Code)
		}
	}
	return "watch error"
}

func toWatchEvent(ev watch.Event) WatchEvent {
	we := WatchEvent{Type: string(ev.Type)}
	if u, ok := ev.Object.(*unstructured.Unstructured); ok {
		we.Object = u.Object
	}
	return we
}
