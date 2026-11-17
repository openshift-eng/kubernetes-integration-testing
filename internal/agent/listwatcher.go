package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
)

// ListWatcher implements watch.Interface with list-then-watch semantics
// and automatic reconnection. It lists current objects (delivered as ADDED
// events), then watches from the list's resourceVersion. On watch close,
// it re-lists and re-watches.
type ListWatcher struct {
	ctx       context.Context
	cancel    context.CancelFunc
	kc        KubeClient
	gvr       schema.GroupVersionResource
	namespace string
	opts      metav1.ListOptions
	events    chan watch.Event
	log       *slog.Logger
	stopOnce  sync.Once
}

func NewListWatcher(ctx context.Context, kc KubeClient, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions, log *slog.Logger) *ListWatcher {
	ctx, cancel := context.WithCancel(ctx)
	lw := &ListWatcher{
		ctx:       ctx,
		cancel:    cancel,
		kc:        kc,
		gvr:       gvr,
		namespace: namespace,
		opts:      opts,
		events:    make(chan watch.Event),
		log:       log,
	}
	go lw.run()
	return lw
}

func (lw *ListWatcher) ResultChan() <-chan watch.Event {
	return lw.events
}

func (lw *ListWatcher) Stop() {
	lw.stopOnce.Do(func() {
		lw.cancel()
	})
}

func (lw *ListWatcher) run() {
	defer close(lw.events)

	for {
		if lw.ctx.Err() != nil {
			return
		}

		list, err := lw.kc.List(lw.ctx, lw.gvr, lw.namespace, lw.opts)
		if err != nil {
			if lw.ctx.Err() != nil {
				return
			}
			if kube.IsTerminalError(err) {
				lw.log.Error("list failed with terminal error", "gvr", lw.gvr.String(), "error", err)
				lw.sendError(err)
				return
			}
			lw.log.Warn("list failed, retrying", "gvr", lw.gvr.String(), "error", err)
			select {
			case <-time.After(time.Second):
			case <-lw.ctx.Done():
				return
			}
			continue
		}

		for i := range list.Items {
			select {
			case lw.events <- watch.Event{Type: watch.Added, Object: &list.Items[i]}:
			case <-lw.ctx.Done():
				return
			}
		}

		watchOpts := lw.opts
		watchOpts.ResourceVersion = list.GetResourceVersion()
		watchOpts.AllowWatchBookmarks = true

		w, err := lw.kc.Watch(lw.ctx, lw.gvr, lw.namespace, watchOpts)
		if err != nil {
			if lw.ctx.Err() != nil {
				return
			}
			if kube.IsTerminalError(err) {
				lw.log.Error("watch failed with terminal error", "gvr", lw.gvr.String(), "error", err)
				lw.sendError(err)
				return
			}
			lw.log.Warn("watch failed, retrying", "gvr", lw.gvr.String(), "error", err)
			select {
			case <-time.After(time.Second):
			case <-lw.ctx.Done():
				return
			}
			continue
		}

		action := lw.forwardEvents(w)
		if action == reconnectStop {
			return
		}
		lw.log.Info("watch closed, reconnecting", "gvr", lw.gvr.String())
		if action == reconnectBackoff {
			select {
			case <-time.After(time.Second):
			case <-lw.ctx.Done():
				return
			}
		}
	}
}

func (lw *ListWatcher) sendError(err error) {
	status := &metav1.Status{
		Status:  metav1.StatusFailure,
		Message: err.Error(),
	}
	select {
	case lw.events <- watch.Event{Type: watch.Error, Object: status}:
	case <-lw.ctx.Done():
	}
}

type reconnectAction int

const (
	reconnectStop      reconnectAction = iota // don't reconnect
	reconnectImmediate                        // reconnect now (expired, normal close)
	reconnectBackoff                          // reconnect after delay (transient error)
)

func (lw *ListWatcher) forwardEvents(w watch.Interface) reconnectAction {
	for {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				return reconnectImmediate
			}
			if ev.Type == watch.Bookmark {
				continue
			}
			if ev.Type == watch.Error {
				apiErr := apierrors.FromObject(ev.Object)
				if apierrors.IsResourceExpired(apiErr) {
					lw.log.Info("watch expired, reconnecting", "gvr", lw.gvr.String())
					w.Stop()
					return reconnectImmediate
				}
				if kube.IsTerminalError(apiErr) {
					select {
					case lw.events <- ev:
					case <-lw.ctx.Done():
					}
					w.Stop()
					return reconnectStop
				}
				lw.log.Warn("transient watch error, reconnecting", "gvr", lw.gvr.String(), "error", apiErr)
				w.Stop()
				return reconnectBackoff
			}
			select {
			case lw.events <- ev:
			case <-lw.ctx.Done():
				w.Stop()
				return reconnectStop
			}
		case <-lw.ctx.Done():
			w.Stop()
			return reconnectStop
		}
	}
}
