package goja

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
)

func TestRuntime(t *testing.T) {
	t.Run("exit code 0", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { return {exitCode: 0}; };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("exit code 1 with reason", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { return {exitCode: 1, reason: "fail"}; };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 1, result.ExitCode)
		assert.Equal(t, "fail", result.Reason)
	})

	t.Run("undefined return is success", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) {};`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("module.exports function", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `module.exports = function(ctx) { return {exitCode: 0}; };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("exports.main function", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { return {exitCode: 0, reason: "ok"}; };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
		assert.Equal(t, "ok", result.Reason)
	})
}

func TestErrors(t *testing.T) {
	t.Run("missing entrypoint", func(t *testing.T) {
		_, err := runScript(t, map[string]string{}, "pkg/main.js")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "entrypoint")
	})

	t.Run("syntax error", func(t *testing.T) {
		_, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) {{{`,
		}, "pkg/main.js")
		require.Error(t, err)
	})

	t.Run("missing main export", func(t *testing.T) {
		_, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.helper = function() {};`,
		}, "pkg/main.js")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "main")
	})

	t.Run("script throws", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { throw new Error("boom"); };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 1, result.ExitCode)
		assert.Contains(t, result.Reason, "boom")
	})

	t.Run("context cancellation", func(t *testing.T) {
		dir := writeBundle(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { while(true) {} };`,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		req := agent.AgentRequest{
			BundleDir:  dir,
			Entrypoint: "pkg/main.js",
			Pod:        agent.PodReference{Name: "p"},
		}
		_, err := New(5*time.Second).Run(ctx, req, &mockHost{})
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
