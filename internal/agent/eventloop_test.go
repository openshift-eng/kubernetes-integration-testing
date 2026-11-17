package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
)

type mockHandler struct {
	mu     sync.Mutex
	events []WatchEvent
	result AgentResult
	stop   bool
	onCall func(WatchEvent)
}

func (h *mockHandler) OnEvent(event WatchEvent) (AgentResult, bool) {
	h.mu.Lock()
	h.events = append(h.events, event)
	result, stop := h.result, h.stop
	cb := h.onCall
	h.mu.Unlock()
	if cb != nil {
		cb(event)
	}
	return result, stop
}

func (h *mockHandler) getEvents() []WatchEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]WatchEvent, len(h.events))
	copy(out, h.events)
	return out
}

func TestEventLoopRunNoWatches(t *testing.T) {
	el := NewEventLoop(context.Background())
	rr := el.Run()
	assert.Equal(t, OutcomeNone, rr.Outcome)
	assert.Equal(t, AgentResult{}, rr.Result)
}

func TestEventLoopDispatchesEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	el := NewEventLoop(ctx)

	fw := watch.NewFake()
	handler := &mockHandler{stop: true}
	require.NoError(t, el.AddWatch(fw, handler))

	go func() {
		fw.Add(&unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "cm-1"},
		}})
	}()

	rr := el.Run()
	require.Equal(t, OutcomeTerminal, rr.Outcome)
	assert.Equal(t, AgentResult{}, rr.Result)

	events := handler.getEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "ADDED", events[0].Type)
}

func TestEventLoopHandlerStopsLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	el := NewEventLoop(ctx)

	fw := watch.NewFake()
	handler := &mockHandler{result: AgentResult{ExitCode: 42, Reason: "done"}, stop: true}
	require.NoError(t, el.AddWatch(fw, handler))

	go func() {
		fw.Modify(&unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "x"},
		}})
	}()

	rr := el.Run()
	require.Equal(t, OutcomeTerminal, rr.Outcome)
	assert.Equal(t, 42, rr.Result.ExitCode)
	assert.Equal(t, "done", rr.Result.Reason)
}

func TestEventLoopContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	el := NewEventLoop(ctx)

	fw := watch.NewFake()
	handler := &mockHandler{}
	require.NoError(t, el.AddWatch(fw, handler))

	cancel()

	rr := el.Run()
	require.Equal(t, OutcomeCanceled, rr.Outcome)
}

func TestEventLoopWatchChannelClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	el := NewEventLoop(ctx)

	fw1 := watch.NewFake()
	fw2 := watch.NewFake()
	handler1 := &mockHandler{}
	handler2 := &mockHandler{stop: true}
	require.NoError(t, el.AddWatch(fw1, handler1))
	require.NoError(t, el.AddWatch(fw2, handler2))

	go func() {
		fw1.Stop()
		fw2.Add(&unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "after-close"},
		}})
	}()

	rr := el.Run()
	require.Equal(t, OutcomeTerminal, rr.Outcome)
	assert.Equal(t, AgentResult{}, rr.Result)

	events := handler2.getEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "after-close", events[0].Object["metadata"].(map[string]interface{})["name"])
}

func TestEventLoopAddWatchDuringHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	el := NewEventLoop(ctx)

	fw1 := watch.NewFake()
	fw2 := watch.NewFake()

	callCount := 0
	handler2 := &mockHandler{stop: true}

	handler1 := &mockHandler{
		onCall: func(event WatchEvent) {
			callCount++
			if callCount == 1 {
				_ = el.AddWatch(fw2, handler2)
				go func() {
					fw2.Add(&unstructured.Unstructured{Object: map[string]interface{}{
						"metadata": map[string]interface{}{"name": "dynamic"},
					}})
				}()
			}
		},
	}
	assert.NoError(t, el.AddWatch(fw1, handler1))

	go func() {
		fw1.Add(&unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "trigger"},
		}})
	}()

	rr := el.Run()
	require.Equal(t, OutcomeTerminal, rr.Outcome)
	assert.Equal(t, AgentResult{}, rr.Result)

	events := handler2.getEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "dynamic", events[0].Object["metadata"].(map[string]interface{})["name"])
}

func TestEventLoopStopUnblocksPumps(t *testing.T) {
	el := NewEventLoop(context.Background())

	fw1 := watch.NewFake()
	fw2 := watch.NewFake()
	handler1 := &mockHandler{stop: true}
	handler2 := &mockHandler{}
	require.NoError(t, el.AddWatch(fw1, handler1))
	require.NoError(t, el.AddWatch(fw2, handler2))

	done := make(chan struct{})
	go func() {
		el.Run()
		close(done)
	}()

	fw2.Add(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "e1"},
	}})

	fw1.Add(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "stop"},
	}})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pumps blocked after terminal stop")
	}
}

func TestEventLoopWatchErrorStopsWithFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	el := NewEventLoop(ctx)

	fw := watch.NewFake()
	handler := &mockHandler{}
	require.NoError(t, el.AddWatch(fw, handler))

	go func() {
		fw.Error(&metav1.Status{
			Status:  metav1.StatusFailure,
			Message: "configmaps is forbidden",
		})
	}()

	rr := el.Run()
	require.Equal(t, OutcomeWatchError, rr.Outcome)
	assert.Equal(t, 1, rr.Result.ExitCode)
	assert.Equal(t, "configmaps is forbidden", rr.Result.Reason)
	assert.Empty(t, handler.getEvents(), "ERROR event should not dispatch to handler")
}

func TestEventLoopAddWatchAfterStop(t *testing.T) {
	el := NewEventLoop(context.Background())
	el.Stop()

	fw := watch.NewFake()
	err := el.AddWatch(fw, &mockHandler{})
	require.Error(t, err)
	assert.True(t, fw.IsStopped(), "rejected watcher should be stopped")
}

func TestEventLoopCancellationBeatsClosedRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	el := NewEventLoop(ctx)

	fw := watch.NewFake()
	handler := &mockHandler{}
	require.NoError(t, el.AddWatch(fw, handler))

	cancel()
	fw.Stop()

	rr := el.Run()
	require.Equal(t, OutcomeCanceled, rr.Outcome)
}
