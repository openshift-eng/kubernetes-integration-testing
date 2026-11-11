package goja

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clienttesting "k8s.io/client-go/testing"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
)

func TestWatchOneShotNoRegression(t *testing.T) {
	host := newKubeHost()
	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
exports.main = function(ctx) {
  return {exitCode: 0, reason: "one-shot"};
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "one-shot", result.Reason)
}

func TestWatchCtxRunNoWatches(t *testing.T) {
	host := newKubeHost()
	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
exports.main = function(ctx) {
  ctx.run();
  return {exitCode: 0, reason: "no watches"};
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "no watches", result.Reason)
}

func TestWatchReceivesEvents(t *testing.T) {
	f := newKubeFixture()

	go func() {
		time.Sleep(50 * time.Millisecond)
		gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
		f.fake.Tracker().Create(gvr, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "watch-cm", "namespace": "default"},
		}}, "default")
	}()

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  var received = [];
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {
      received.push(event.type + ":" + event.object.metadata.name);
      return {exitCode: 0, reason: received.join(",")};
    }
  );
  return ctx.run();
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Reason, "watch-cm")
}

func TestWatchCallbackStopsLoop(t *testing.T) {
	f := newKubeFixture()

	go func() {
		time.Sleep(50 * time.Millisecond)
		gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
		for i := 0; i < 3; i++ {
			f.fake.Tracker().Create(gvr, &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "cm-" + string(rune('a'+i)),
					"namespace": "default",
				},
			}}, "default")
			time.Sleep(10 * time.Millisecond)
		}
	}()

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  var count = 0;
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {
      count++;
      if (count >= 2) return {exitCode: 42, reason: "stopped after " + count};
    }
  );
  return ctx.run();
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 42, result.ExitCode)
	assert.Equal(t, "stopped after 2", result.Reason)
}

func TestWatchContextCancellation(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestWatchOnShutdown(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  var shutdownCalled = false;
  ctx.onShutdown(function() {
    shutdownCalled = true;
  });
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestWatchOnShutdownWithSleep(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  ctx.onShutdown(function() {
    ctx.sleep(10);
    ctx.log("shutdown-done");
  });
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	logs := f.host.getLogs()
	shutdownCount := 0
	for _, l := range logs {
		if l == "shutdown-done" {
			shutdownCount++
		}
	}
	assert.Equal(t, 1, shutdownCount, "onShutdown callback should execute exactly once")
}

func TestWatchPreCancelledCtxRunNoWatches(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
exports.main = function(ctx) {
  ctx.run();
  return {exitCode: 99, reason: "should not reach"};
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestWatchCancelledSleepThenExit(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
exports.main = function(ctx) {
  ctx.sleep(5000);
  ctx.exit(42, "should not win");
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode, "cancellation wins over ctx.exit()")
}

func TestWatchOnShutdownCallsExit(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  ctx.onShutdown(function() {
    ctx.exit(99, "from shutdown");
  });
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode, "ctx.exit() inside onShutdown must not override cancellation")
}

func TestWatchOnShutdownCallsRun(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  ctx.onShutdown(function() {
    ctx.run();
  });
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	type runResult struct {
		result agent.AgentResult
		err    error
	}
	ch := make(chan runResult, 1)
	go func() {
		r, e := New(5*time.Second).Run(ctx, req, f.host)
		ch <- runResult{r, e}
	}()

	select {
	case rr := <-ch:
		require.NoError(t, rr.err)
		assert.Equal(t, 0, rr.result.ExitCode)
	case <-time.After(5 * time.Second):
		t.Fatal("onShutdown calling ctx.run() caused a deadlock")
	}
}

func TestWatchOnShutdownInfiniteLoopInterrupted(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  ctx.onShutdown(function() {
    while (true) {}
  });
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	type runResult struct {
		result agent.AgentResult
		err    error
	}
	ch := make(chan runResult, 1)
	go func() {
		r, e := New(100*time.Millisecond).Run(ctx, req, f.host)
		ch <- runResult{r, e}
	}()

	select {
	case rr := <-ch:
		require.NoError(t, rr.err)
		assert.Equal(t, 0, rr.result.ExitCode)
	case <-time.After(5 * time.Second):
		t.Fatal("infinite onShutdown loop was not interrupted by grace period")
	}
}

func TestWatchKubeCallDuringCancellation(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  ctx.sleep(5000);
  kube.get({group: "", version: "v1", resource: "configmaps", namespace: "default", name: "x"});
  return {exitCode: 99, reason: "should not reach"};
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode, "cancellation wins over in-flight kube call")
}

func TestWatchTerminalErrorEndToEnd(t *testing.T) {
	f := newKubeFixture()
	f.fake.PrependWatchReactor("configmaps", func(action clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "configmaps"}, "", nil,
		)
	})

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 1, result.ExitCode)
	assert.Contains(t, result.Reason, "forbidden")
}

func TestWatchOnShutdownKubeGetSucceeds(t *testing.T) {
	existingCM := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "shutdown-cm", "namespace": "default"},
		"data":     map[string]interface{}{"key": "value"},
	}}
	f := newKubeFixture(existingCM)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  ctx.onShutdown(function() {
    var cm = kube.get({group: "", version: "v1", resource: "configmaps", namespace: "default", name: "shutdown-cm"});
    ctx.log("got:" + cm.data.key);
  });
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	logs := f.host.getLogs()
	assert.Contains(t, logs, "got:value", "kube.get() should succeed during onShutdown")
}

func TestWatchOnShutdownKubeWatchThrows(t *testing.T) {
	f := newKubeFixture()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	dir := writeBundle(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  ctx.onShutdown(function() {
    try {
      kube.watch(
        {group: "", version: "v1", resource: "configmaps", namespace: "default"},
        function(event) {}
      );
      ctx.log("should-not-reach");
    } catch(e) {
      ctx.log("caught:" + e.toString());
    }
  });
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {}
  );
  return ctx.run();
};`,
	})

	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: "pkg/main.js",
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}

	actionsBeforeRun := len(f.fake.Actions())

	result, err := New(5*time.Second).Run(ctx, req, f.host)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	logs := f.host.getLogs()
	foundCatch := false
	for _, l := range logs {
		if l == "should-not-reach" {
			t.Fatal("kube.watch() during shutdown should throw, not succeed")
		}
		if len(l) > 7 && l[:7] == "caught:" {
			foundCatch = true
		}
	}
	assert.True(t, foundCatch, "kube.watch() during shutdown should throw a catchable error")

	var listCount, watchCount int
	for _, a := range f.fake.Actions()[actionsBeforeRun:] {
		switch a.GetVerb() {
		case "list":
			listCount++
		case "watch":
			watchCount++
		}
	}
	assert.LessOrEqual(t, listCount, 1, "at most one list from the initial subscription")
	assert.LessOrEqual(t, watchCount, 1, "at most one watch from the initial subscription")
}

func newKubeFixtureWithObjects(objects ...runtime.Object) *kubeFixture {
	return newKubeFixture(objects...)
}

func TestWatchListThenWatch(t *testing.T) {
	existingCM := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "existing-cm", "namespace": "default"},
	}}
	f := newKubeFixtureWithObjects(existingCM)

	go func() {
		time.Sleep(100 * time.Millisecond)
		gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
		f.fake.Tracker().Create(gvr, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "new-cm", "namespace": "default"},
		}}, "default")
	}()

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  var events = [];
  kube.watch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default"},
    function(event) {
      events.push(event.object.metadata.name);
      if (events.length >= 2) return {exitCode: 0, reason: events.join(",")};
    }
  );
  return ctx.run();
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Reason, "existing-cm")
	assert.Contains(t, result.Reason, "new-cm")
}
