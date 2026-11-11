package goja

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
)

func TestCtx(t *testing.T) {
	t.Run("log writes to host", func(t *testing.T) {
		dir := writeBundle(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { ctx.log("hello"); ctx.log("world"); };`,
		})
		host := &mockHost{}
		req := agent.AgentRequest{
			BundleDir:  dir,
			Entrypoint: "pkg/main.js",
			Pod:        agent.PodReference{Name: "p"},
		}
		_, err := New(5*time.Second).Run(context.Background(), req, host)
		require.NoError(t, err)
		assert.Equal(t, []string{"hello", "world"}, host.getLogs())
	})

	t.Run("sleep completes normally", func(t *testing.T) {
		start := time.Now()
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { ctx.sleep(50); return {exitCode: 0}; };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
		assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)
	})

	t.Run("sleep respects cancellation", func(t *testing.T) {
		dir := writeBundle(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { ctx.sleep(60000); };`,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
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

	t.Run("values accessible", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) {
				if (ctx.values.key !== "value") return {exitCode: 1, reason: "wrong key"};
				if (ctx.values.count !== 3) return {exitCode: 1, reason: "wrong count"};
				return {exitCode: 0};
			};`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("pod populated", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) {
				if (ctx.pod.name !== "test-pod") return {exitCode: 1, reason: "name"};
				if (ctx.pod.namespace !== "test-ns") return {exitCode: 1, reason: "ns"};
				if (ctx.pod.uid !== "uid-123") return {exitCode: 1, reason: "uid"};
				if (ctx.pod.nodeName !== "node-1") return {exitCode: 1, reason: "node"};
				return {exitCode: 0};
			};`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("env allowed", func(t *testing.T) {
		t.Setenv("KIT_TEST_VAR", "hello")
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) {
				var v = ctx.env("KIT_TEST_VAR");
				if (v !== "hello") return {exitCode: 1, reason: "got: " + v};
				return {exitCode: 0};
			};`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("env blocked", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) {
				var v = ctx.env("HOME");
				if (v !== undefined) return {exitCode: 1, reason: "should be undefined"};
				return {exitCode: 0};
			};`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("exit terminates", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) {
				ctx.exit(42, "done");
				throw new Error("should not reach");
			};`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 42, result.ExitCode)
		assert.Equal(t, "done", result.Reason)
	})

	t.Run("exit without reason", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `exports.main = function(ctx) { ctx.exit(0); };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
		assert.Empty(t, result.Reason)
	})

	t.Run("exit from nested module", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js":   `var helper = require("./helper"); exports.main = function(ctx) { helper.bail(ctx); };`,
			"pkg/helper.js": `exports.bail = function(ctx) { ctx.exit(7, "nested"); };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 7, result.ExitCode)
		assert.Equal(t, "nested", result.Reason)
	})
}
