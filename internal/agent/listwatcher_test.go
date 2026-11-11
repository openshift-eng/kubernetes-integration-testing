package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

var testGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

type stubKubeClient struct {
	listFn  func(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	watchFn func(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (watch.Interface, error)
}

func (s *stubKubeClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (s *stubKubeClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return s.listFn(ctx, gvr, ns, opts)
}
func (s *stubKubeClient) Create(context.Context, schema.GroupVersionResource, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (s *stubKubeClient) Update(context.Context, schema.GroupVersionResource, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (s *stubKubeClient) Patch(context.Context, schema.GroupVersionResource, string, string, types.PatchType, []byte, string) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (s *stubKubeClient) Apply(context.Context, schema.GroupVersionResource, *unstructured.Unstructured, string) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (s *stubKubeClient) Delete(context.Context, schema.GroupVersionResource, string, string) error {
	panic("unused")
}
func (s *stubKubeClient) Watch(ctx context.Context, gvr schema.GroupVersionResource, ns string, opts metav1.ListOptions) (watch.Interface, error) {
	return s.watchFn(ctx, gvr, ns, opts)
}
func (s *stubKubeClient) ResolveGVR(schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	panic("unused")
}

func stubItem(name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": name, "namespace": "default"},
	}}
}

func recvEvent(t *testing.T, ch <-chan watch.Event) watch.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		require.True(t, ok, "channel closed unexpectedly")
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for event")
		return watch.Event{}
	}
}

func TestListWatcherDeliversListItems(t *testing.T) {
	items := []unstructured.Unstructured{stubItem("a"), stubItem("b")}
	fw := watch.NewFake()

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{Items: items}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	ev1 := recvEvent(t, lw.ResultChan())
	assert.Equal(t, watch.Added, ev1.Type)
	assert.Equal(t, "a", ev1.Object.(*unstructured.Unstructured).GetName())

	ev2 := recvEvent(t, lw.ResultChan())
	assert.Equal(t, watch.Added, ev2.Type)
	assert.Equal(t, "b", ev2.Object.(*unstructured.Unstructured).GetName())
}

func TestListWatcherListThenWatch(t *testing.T) {
	items := []unstructured.Unstructured{stubItem("listed")}
	fw := watch.NewFake()

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{Items: items}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	ev := recvEvent(t, lw.ResultChan())
	assert.Equal(t, "listed", ev.Object.(*unstructured.Unstructured).GetName())

	watched := stubItem("watched")
	fw.Add(&watched)

	ev = recvEvent(t, lw.ResultChan())
	assert.Equal(t, watch.Added, ev.Type)
	assert.Equal(t, "watched", ev.Object.(*unstructured.Unstructured).GetName())
}

func TestListWatcherReconnectsOnWatchClose(t *testing.T) {
	items := []unstructured.Unstructured{stubItem("obj")}
	fw1 := watch.NewFake()
	fw2 := watch.NewFake()
	watchCh := make(chan watch.Interface, 2)
	watchCh <- fw1
	watchCh <- fw2

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{Items: items}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return <-watchCh, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	// First list delivers "obj"
	ev := recvEvent(t, lw.ResultChan())
	assert.Equal(t, "obj", ev.Object.(*unstructured.Unstructured).GetName())

	// Close first watch — triggers reconnection
	fw1.Stop()

	// Second list delivers "obj" again (re-list)
	ev = recvEvent(t, lw.ResultChan())
	assert.Equal(t, "obj", ev.Object.(*unstructured.Unstructured).GetName())

	// Second watch receives new events
	newObj := stubItem("new-obj")
	fw2.Add(&newObj)

	ev = recvEvent(t, lw.ResultChan())
	assert.Equal(t, "new-obj", ev.Object.(*unstructured.Unstructured).GetName())
}

func TestListWatcherContextCancellation(t *testing.T) {
	fw := watch.NewFake()

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)

	cancel()

	select {
	case _, ok := <-lw.ResultChan():
		if ok {
			// Might drain one event before close, that's fine
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed after context cancellation")
	}
}

func TestListWatcherListErrorRetries(t *testing.T) {
	var listCalls atomic.Int32
	items := []unstructured.Unstructured{stubItem("after-retry")}
	fw := watch.NewFake()

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			if listCalls.Add(1) == 1 {
				return nil, fmt.Errorf("transient error")
			}
			return &unstructured.UnstructuredList{Items: items}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	ev := recvEvent(t, lw.ResultChan())
	assert.Equal(t, "after-retry", ev.Object.(*unstructured.Unstructured).GetName())
	assert.True(t, listCalls.Load() >= 2)
}

func TestListWatcherBookmarksSkipped(t *testing.T) {
	fw := watch.NewFake()

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	// Send a bookmark — should be skipped
	fw.Action(watch.Bookmark, &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"resourceVersion": "100"},
	}})

	// Send a real event after the bookmark
	real := stubItem("real")
	fw.Add(&real)

	ev := recvEvent(t, lw.ResultChan())
	assert.Equal(t, watch.Added, ev.Type)
	assert.Equal(t, "real", ev.Object.(*unstructured.Unstructured).GetName())
}

func TestListWatcherPassesResourceVersion(t *testing.T) {
	items := &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{stubItem("a")},
	}
	items.SetResourceVersion("42")

	var watchRV string
	fw := watch.NewFake()

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return items, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, opts metav1.ListOptions) (watch.Interface, error) {
			watchRV = opts.ResourceVersion
			return fw, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	recvEvent(t, lw.ResultChan())
	// Give the goroutine time to open the watch
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, "42", watchRV)
}

func TestListWatcherTerminalListError(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"Forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", nil)},
		{"Unauthorized", apierrors.NewUnauthorized("bad token")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kc := &stubKubeClient{
				listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
					return nil, tt.err
				},
			}

			lw := NewListWatcher(context.Background(), kc, testGVR, "default", metav1.ListOptions{}, discardLog)
			defer lw.Stop()

			ev := recvEvent(t, lw.ResultChan())
			assert.Equal(t, watch.Error, ev.Type)
			status, ok := ev.Object.(*metav1.Status)
			require.True(t, ok, "error event should carry *metav1.Status")
			assert.NotEmpty(t, status.Message)

			// Channel closes after the error event
			select {
			case _, ok := <-lw.ResultChan():
				assert.False(t, ok, "channel should be closed after error event")
			case <-time.After(5 * time.Second):
				t.Fatal("timeout: channel should close after error event")
			}
		})
	}
}

func TestListWatcherTerminalWatchError(t *testing.T) {
	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", nil)
		},
	}

	lw := NewListWatcher(context.Background(), kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	ev := recvEvent(t, lw.ResultChan())
	assert.Equal(t, watch.Error, ev.Type)
	status, ok := ev.Object.(*metav1.Status)
	require.True(t, ok)
	assert.NotEmpty(t, status.Message)
}

func TestListWatcherTransientErrorStillRetries(t *testing.T) {
	var listCalls atomic.Int32
	fw := watch.NewFake()

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			if listCalls.Add(1) == 1 {
				return nil, apierrors.NewServiceUnavailable("try again")
			}
			return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{stubItem("ok")}}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	ev := recvEvent(t, lw.ResultChan())
	assert.Equal(t, "ok", ev.Object.(*unstructured.Unstructured).GetName())
	assert.True(t, listCalls.Load() >= 2)
}

func TestListWatcherExpiredWatchReconnects(t *testing.T) {
	fw1 := watch.NewFake()
	fw2 := watch.NewFake()
	watchCh := make(chan watch.Interface, 2)
	watchCh <- fw1
	watchCh <- fw2

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{stubItem("obj")}}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return <-watchCh, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	// Drain the first LIST's ADDED event
	recvEvent(t, lw.ResultChan())

	// Send an Expired error through the watch — should reconnect, not propagate
	fw1.Error(&metav1.Status{
		Status:  metav1.StatusFailure,
		Reason:  metav1.StatusReasonExpired,
		Message: "resourceVersion too old",
	})

	// After reconnection, the re-list delivers the item again
	ev := recvEvent(t, lw.ResultChan())
	assert.Equal(t, watch.Added, ev.Type)
	assert.Equal(t, "obj", ev.Object.(*unstructured.Unstructured).GetName())
}

func TestListWatcherTransientWatchErrorReconnects(t *testing.T) {
	fw1 := watch.NewFake()
	fw2 := watch.NewFake()
	watchCh := make(chan watch.Interface, 2)
	watchCh <- fw1
	watchCh <- fw2

	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{stubItem("obj")}}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return <-watchCh, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lw := NewListWatcher(ctx, kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	defer lw.Stop()

	recvEvent(t, lw.ResultChan())

	// Send an InternalServerError through the watch — should reconnect
	fw1.Error(&metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    500,
		Message: "internal error",
	})

	// Re-list delivers the item again
	ev := recvEvent(t, lw.ResultChan())
	assert.Equal(t, watch.Added, ev.Type)
	assert.Equal(t, "obj", ev.Object.(*unstructured.Unstructured).GetName())
}

func TestListWatcherStopIdempotent(t *testing.T) {
	kc := &stubKubeClient{
		listFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
			return &unstructured.UnstructuredList{}, nil
		},
		watchFn: func(_ context.Context, _ schema.GroupVersionResource, _ string, _ metav1.ListOptions) (watch.Interface, error) {
			return watch.NewFake(), nil
		},
	}

	lw := NewListWatcher(context.Background(), kc, testGVR, "default", metav1.ListOptions{}, discardLog)
	lw.Stop()
	lw.Stop() // must not panic
}
