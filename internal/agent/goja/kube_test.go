package goja

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/kube"
)

func kubeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	for _, kind := range []string{"ConfigMap", "Pod"} {
		scheme.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: "", Version: "v1", Kind: kind},
			&unstructured.Unstructured{},
		)
		scheme.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: "", Version: "v1", Kind: kind + "List"},
			&unstructured.UnstructuredList{},
		)
	}
	return scheme
}

func kubeMapper() *meta.DefaultRESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	return mapper
}

func newKubeHost(objects ...runtime.Object) *mockHost {
	dynClient := k8stesting.NewSimpleDynamicClient(kubeScheme(), objects...)
	return &mockHost{kc: kube.NewClient(dynClient, kubeMapper(), kube.NewPolicy())}
}

type kubeFixture struct {
	host *mockHost
	fake *k8stesting.FakeDynamicClient
}

func newKubeFixture(objects ...runtime.Object) *kubeFixture {
	dynClient := k8stesting.NewSimpleDynamicClient(kubeScheme(), objects...)
	return &kubeFixture{
		host: &mockHost{kc: kube.NewClient(dynClient, kubeMapper(), kube.NewPolicy())},
		fake: dynClient,
	}
}

type patchRecord struct {
	mu          sync.Mutex
	patchType   types.PatchType
	subresource string
}

func (p *patchRecord) get() (types.PatchType, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.patchType, p.subresource
}

func runWithKube(t *testing.T, files map[string]string, entrypoint string, host *mockHost) (agent.AgentResult, error) {
	t.Helper()
	dir := writeBundle(t, files)
	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: entrypoint,
		Lifecycle:  agent.LifecycleBounded,
		Values:     map[string]any{},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}
	return New().Run(context.Background(), req, host)
}

func TestKubeCreateAndGet(t *testing.T) {
	host := newKubeHost()

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.create({
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: {name: "my-cm", namespace: "default"},
    data: {key: "value"}
  });
  var cm = kube.get({group: "", version: "v1", resource: "configmaps", namespace: "default", name: "my-cm"});
  if (cm.data.key !== "value") return {exitCode: 1, reason: "wrong data: " + cm.data.key};
  return {exitCode: 0};
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestKubeList(t *testing.T) {
	host := newKubeHost(
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm-1", "namespace": "default"},
		}},
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm-2", "namespace": "default"},
		}},
	)

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  var cms = kube.list({group: "", version: "v1", resource: "configmaps", namespace: "default"});
  if (cms.length !== 2) return {exitCode: 1, reason: "expected 2, got " + cms.length};
  return {exitCode: 0};
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestKubeReplace(t *testing.T) {
	host := newKubeHost(
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "my-cm", "namespace": "default"},
			"data":     map[string]interface{}{"old": "value"},
		}},
	)

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.replace({
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: {name: "my-cm", namespace: "default"},
    data: {new: "value"}
  });
  var cm = kube.get({group: "", version: "v1", resource: "configmaps", namespace: "default", name: "my-cm"});
  if (cm.data["new"] !== "value") return {exitCode: 1, reason: "replace failed"};
  return {exitCode: 0};
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestKubeDelete(t *testing.T) {
	host := newKubeHost(
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "to-delete", "namespace": "default"},
		}},
	)

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube["delete"]({group: "", version: "v1", resource: "configmaps", namespace: "default", name: "to-delete"});
  try {
    kube.get({group: "", version: "v1", resource: "configmaps", namespace: "default", name: "to-delete"});
    return {exitCode: 1, reason: "should have been deleted"};
  } catch(e) {
    return {exitCode: 0};
  }
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestKubeErrorBubblesAsException(t *testing.T) {
	host := newKubeHost()

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  try {
    kube.get({group: "", version: "v1", resource: "configmaps", namespace: "default", name: "nonexistent"});
    return {exitCode: 1, reason: "should have thrown"};
  } catch(e) {
    return {exitCode: 0};
  }
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestKubePolicyDeniesSecrets(t *testing.T) {
	host := newKubeHost()

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  try {
    kube.get({group: "", version: "v1", resource: "secrets", namespace: "default", name: "my-secret"});
    return {exitCode: 1, reason: "should have been denied"};
  } catch(e) {
    if (String(e).indexOf("denied") === -1) return {exitCode: 1, reason: "wrong error: " + e};
    return {exitCode: 0};
  }
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestKubeResolveGVR(t *testing.T) {
	host := newKubeHost()

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  var gvr = kube.resolveGVR({group: "", version: "v1", kind: "ConfigMap"});
  if (gvr.resource !== "configmaps") return {exitCode: 1, reason: "got " + gvr.resource};
  return {exitCode: 0};
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestKubePatchMerge(t *testing.T) {
	f := newKubeFixture(
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
			"data":     map[string]interface{}{"a": "1"},
		}},
	)

	rec := &patchRecord{}
	f.fake.PrependReactor("patch", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchAction)
		rec.mu.Lock()
		rec.patchType = pa.GetPatchType()
		rec.subresource = pa.GetSubresource()
		rec.mu.Unlock()
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
		}}, nil
	})

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.patch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default", name: "cm"},
    {data: {b: "2"}}
  );
  return {exitCode: 0};
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	pt, sub := rec.get()
	assert.Equal(t, types.MergePatchType, pt)
	assert.Empty(t, sub)
}

func TestKubePatchStrategic(t *testing.T) {
	f := newKubeFixture(
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
		}},
	)

	rec := &patchRecord{}
	f.fake.PrependReactor("patch", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchAction)
		rec.mu.Lock()
		rec.patchType = pa.GetPatchType()
		rec.mu.Unlock()
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
		}}, nil
	})

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.patch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default", name: "cm"},
    {data: {x: "y"}},
    {type: "strategic"}
  );
  return {exitCode: 0};
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	pt, _ := rec.get()
	assert.Equal(t, types.StrategicMergePatchType, pt)
}

func TestKubePatchJSON(t *testing.T) {
	f := newKubeFixture(
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
		}},
	)

	rec := &patchRecord{}
	f.fake.PrependReactor("patch", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchAction)
		rec.mu.Lock()
		rec.patchType = pa.GetPatchType()
		rec.mu.Unlock()
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
		}}, nil
	})

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.patch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default", name: "cm"},
    [{op: "add", path: "/data/z", value: "1"}],
    {type: "json"}
  );
  return {exitCode: 0};
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	pt, _ := rec.get()
	assert.Equal(t, types.JSONPatchType, pt)
}

func TestKubePatchInvalidType(t *testing.T) {
	host := newKubeHost(
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
		}},
	)

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  try {
    kube.patch(
      {group: "", version: "v1", resource: "configmaps", namespace: "default", name: "cm"},
      {data: {x: "y"}},
      {type: "bogus"}
    );
    return {exitCode: 1, reason: "should have thrown"};
  } catch(e) {
    if (String(e).indexOf("unknown patch type") === -1) return {exitCode: 1, reason: "wrong error: " + e};
    return {exitCode: 0};
  }
};`,
	}, "pkg/main.js", host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}

func TestKubePatchSubresource(t *testing.T) {
	f := newKubeFixture(
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
		}},
	)

	rec := &patchRecord{}
	f.fake.PrependReactor("patch", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchAction)
		rec.mu.Lock()
		rec.subresource = pa.GetSubresource()
		rec.mu.Unlock()
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "cm", "namespace": "default"},
		}}, nil
	})

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.patch(
    {group: "", version: "v1", resource: "configmaps", namespace: "default", name: "cm"},
    {status: {ready: true}},
    {subresource: "status"}
  );
  return {exitCode: 0};
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	_, sub := rec.get()
	assert.Equal(t, "status", sub)
}

func TestKubeApplyWithFieldManager(t *testing.T) {
	f := newKubeFixture()

	f.fake.PrependReactor("patch", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "ssa-cm", "namespace": "default"},
		}}, nil
	})

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  var cm = kube.apply({
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: {name: "ssa-cm", namespace: "default"},
    data: {managed: "true"}
  }, {fieldManager: "custom-manager"});
  if (cm.metadata.name !== "ssa-cm") return {exitCode: 1, reason: "wrong result"};
  return {exitCode: 0};
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	actions := f.fake.Actions()
	var patchAction clienttesting.PatchAction
	for _, a := range actions {
		if pa, ok := a.(clienttesting.PatchAction); ok {
			patchAction = pa
			break
		}
	}
	require.NotNil(t, patchAction)
	assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
}

func TestKubeApplyDefaultFieldManager(t *testing.T) {
	f := newKubeFixture()

	f.fake.PrependReactor("patch", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]interface{}{"name": "ssa-cm", "namespace": "default"},
		}}, nil
	})

	result, err := runWithKube(t, map[string]string{
		"pkg/main.js": `
var kube = require("kube");
exports.main = function(ctx) {
  kube.apply({
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: {name: "ssa-cm", namespace: "default"},
    data: {x: "1"}
  });
  return {exitCode: 0};
};`,
	}, "pkg/main.js", f.host)

	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	actions := f.fake.Actions()
	var patchAction clienttesting.PatchAction
	for _, a := range actions {
		if pa, ok := a.(clienttesting.PatchAction); ok {
			patchAction = pa
			break
		}
	}
	require.NotNil(t, patchAction)
	assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
}
