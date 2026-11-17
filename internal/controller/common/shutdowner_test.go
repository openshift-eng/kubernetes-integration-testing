package common

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShutdownCancelsContext(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	s.Shutdown()

	select {
	case <-s.Context().Done():
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for context cancellation")
	}
}

func TestShutdownSetsDeleting(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	assert.False(t, s.IsDeleting())
	s.Shutdown()
	assert.True(t, s.IsDeleting())
}

func TestIsDeletingFalseOnParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	s := NewDeferredShutdowner(parent)

	cancel()

	select {
	case <-s.Context().Done():
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out")
	}

	assert.False(t, s.IsDeleting())
}

func TestShutdownIsIdempotent(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	s.Shutdown()
	s.Shutdown()

	assert.True(t, s.IsDeleting())
}

func TestShutdownWithErrorCancelsContext(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	s.ShutdownWithError(fmt.Errorf("bundle server died"))

	select {
	case <-s.Context().Done():
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for context cancellation")
	}

	assert.False(t, s.IsDeleting())
}

func TestShutdownWithErrorExposesFailure(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	assert.Nil(t, s.Failure())

	s.ShutdownWithError(fmt.Errorf("bundle server died"))

	assert.ErrorContains(t, s.Failure(), "bundle server died")
}

func TestShutdownHasNoFailure(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	s.Shutdown()

	assert.Nil(t, s.Failure())
}

func TestShutdownWithErrorDoesNotSetDeleting(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	s.ShutdownWithError(fmt.Errorf("crash"))

	assert.False(t, s.IsDeleting())
}

func TestFirstCauseWins(t *testing.T) {
	s := NewDeferredShutdowner(context.Background())

	s.ShutdownWithError(fmt.Errorf("first"))
	s.ShutdownWithError(fmt.Errorf("second"))

	assert.ErrorContains(t, s.Failure(), "first")
}
