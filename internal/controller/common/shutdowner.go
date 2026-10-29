package common

import (
	"context"
	"sync"
)

type Shutdowner interface {
	Shutdown()
}

type DeferredShutdowner struct {
	mu           sync.Mutex
	fn           func(ctx context.Context)
	ctx          context.Context
	shutdownChan chan interface{}
	once         sync.Once
}

func NewDeferredShutdowner(ctx context.Context) *DeferredShutdowner {
	subCtx, subCtxCancel := context.WithCancel(ctx)
	shutdowner := &DeferredShutdowner{
		ctx:          subCtx,
		shutdownChan: make(chan interface{}),
	}
	go func() {
		select {
		case <-subCtx.Done():
		case <-shutdowner.shutdownChan:
		}
		shutdowner.mu.Lock()
		fn := shutdowner.fn
		shutdowner.mu.Unlock()
		if fn != nil {
			fn(ctx)
		}
		subCtxCancel()
	}()
	return shutdowner
}

func (s *DeferredShutdowner) Context() context.Context {
	return s.ctx
}

func (s *DeferredShutdowner) Set(fn func(ctx context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fn = fn
}

func (s *DeferredShutdowner) Shutdown() {
	s.once.Do(func() { close(s.shutdownChan) })
}
