package common

import (
	"context"
	"sync/atomic"
)

type Shutdowner interface {
	Shutdown()
	IsDeleting() bool
}

type DeferredShutdowner struct {
	deleting atomic.Bool
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewDeferredShutdowner(ctx context.Context) *DeferredShutdowner {
	subCtx, cancel := context.WithCancel(ctx)
	return &DeferredShutdowner{ctx: subCtx, cancel: cancel}
}

func (s *DeferredShutdowner) Context() context.Context {
	return s.ctx
}

func (s *DeferredShutdowner) Shutdown() {
	s.deleting.Store(true)
	s.cancel()
}

func (s *DeferredShutdowner) IsDeleting() bool {
	return s.deleting.Load()
}
