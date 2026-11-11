package common

import (
	"context"
	"errors"
	"sync/atomic"
)

type Shutdowner interface {
	Shutdown()
	ShutdownWithError(err error)
	IsDeleting() bool
}

type DeferredShutdowner struct {
	deleting atomic.Bool
	ctx      context.Context
	cancel   context.CancelCauseFunc
}

func NewDeferredShutdowner(ctx context.Context) *DeferredShutdowner {
	subCtx, cancel := context.WithCancelCause(ctx)
	return &DeferredShutdowner{ctx: subCtx, cancel: cancel}
}

func (s *DeferredShutdowner) Context() context.Context {
	return s.ctx
}

func (s *DeferredShutdowner) Shutdown() {
	s.deleting.Store(true)
	s.cancel(nil)
}

func (s *DeferredShutdowner) ShutdownWithError(err error) {
	if err == nil {
		err = errors.New("controller shutdown")
	}
	s.cancel(err)
}

func (s *DeferredShutdowner) IsDeleting() bool {
	return s.deleting.Load()
}

func (s *DeferredShutdowner) Failure() error {
	cause := context.Cause(s.ctx)
	if cause == nil || errors.Is(cause, context.Canceled) {
		return nil
	}
	return cause
}
