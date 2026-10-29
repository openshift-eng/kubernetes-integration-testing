package common

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShutdownRunsFnBeforeCancellingContext(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	var order []string
	done := make(chan struct{})

	s.Set(func(_ context.Context) {
		order = append(order, "fn")
	})

	go func() {
		<-s.Context().Done()
		order = append(order, "ctx_cancelled")
		close(done)
	}()

	s.Shutdown()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)

	assert.Equal(t, []string{"fn", "ctx_cancelled"}, order)
}

func TestShutdownCancelsContextWhenNoFnSet(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	s.Shutdown()

	select {
	case <-s.Context().Done():
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for context cancellation")
	}
}

func TestParentCancelAfterShutdownIsIdempotent(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	s := NewDeferredShutdowner(parent)

	var count atomic.Int32
	s.Set(func(_ context.Context) {
		count.Add(1)
	})

	s.Shutdown()

	select {
	case <-s.Context().Done():
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out")
	}

	parentCancel()

	assert.Equal(t, int32(1), count.Load())
}

func TestParentCancelTriggersFn(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	s := NewDeferredShutdowner(parent)

	var called atomic.Bool
	done := make(chan struct{})

	s.Set(func(_ context.Context) {
		called.Store(true)
		close(done)
	})

	parentCancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for fn")
	}

	assert.True(t, called.Load())
}

func TestFnReceivesParentContext(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	s := NewDeferredShutdowner(parent)

	var fnCtx context.Context
	done := make(chan struct{})

	s.Set(func(ctx context.Context) {
		fnCtx = ctx
		close(done)
	})

	s.Shutdown()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out")
	}

	assert.Equal(t, parent, fnCtx)
}
