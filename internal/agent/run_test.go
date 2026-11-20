package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

type blockingRuntime struct {
	name string
}

func (r *blockingRuntime) Name() string { return r.name }

func (r *blockingRuntime) Run(ctx context.Context, _ AgentRequest, _ Host) (AgentResult, error) {
	<-ctx.Done()
	return AgentResult{}, ctx.Err()
}

func serveBundleHTTP(t *testing.T, b *bundle.Bundle) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		if _, err := io.Copy(w, b.Reader()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	t.Cleanup(srv.Close)
	return srv, fmt.Sprintf("%s/bundle.tar.gz", srv.URL)
}

func buildTestBundle(t *testing.T, timeout *metav1.Duration) *bundle.Bundle {
	t.Helper()
	pkg := collection.ScriptPackageContent{
		Name:       "test-pkg",
		Entrypoint: "main.js",
		Files: []collection.ScriptFile{
			{Path: "main.js", Content: []byte(`exports.main = function(ctx) { ctx.sleep(999999); }`)},
		},
	}
	b, err := bundle.Build(bundle.BuildInput{
		Packages:          []collection.ScriptPackageContent{pkg},
		EntrypointPackage: "test-pkg",
		Runtime:           "js",
		Timeout:           timeout,
	})
	require.NoError(t, err)
	return b
}

func fakeRuntimeFactory(rt Runtime) RuntimeFactory {
	return func(name string) (Runtime, error) {
		if rt.Name() == name {
			return rt, nil
		}
		return nil, fmt.Errorf("unknown runtime %q", name)
	}
}

func TestRunTimeoutExpires(t *testing.T) {
	timeout := &metav1.Duration{Duration: 50 * time.Millisecond}
	b := buildTestBundle(t, timeout)
	_, url := serveBundleHTTP(t, b)

	cfg := Config{BundleURL: url, BundleDigest: b.Digest}
	rt := &blockingRuntime{name: "js"}
	factory := fakeRuntimeFactory(rt)

	err := Run(context.Background(), slog.Default(), afero.NewOsFs(), cfg, factory, &stubKubeClient{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deadline exceeded")
}

func TestRunNoTimeout(t *testing.T) {
	b := buildTestBundle(t, nil)
	_, url := serveBundleHTTP(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	cfg := Config{BundleURL: url, BundleDigest: b.Digest}
	rt := &blockingRuntime{name: "js"}
	factory := fakeRuntimeFactory(rt)

	err := Run(ctx, slog.Default(), afero.NewOsFs(), cfg, factory, &stubKubeClient{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canceled")
	assert.NotContains(t, err.Error(), "deadline")
}

func TestRunNegativeTimeout(t *testing.T) {
	timeout := &metav1.Duration{Duration: -1 * time.Second}
	b := buildTestBundle(t, timeout)
	_, url := serveBundleHTTP(t, b)

	cfg := Config{BundleURL: url, BundleDigest: b.Digest}
	rt := &blockingRuntime{name: "js"}
	factory := fakeRuntimeFactory(rt)

	err := Run(context.Background(), slog.Default(), afero.NewOsFs(), cfg, factory, &stubKubeClient{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent timeout must be positive")
}

type capturingKubeClient struct {
	stubKubeClient
	capturedNamespace string
	capturedCaps      []collection.AtomicCapability
}

func (c *capturingKubeClient) SetCapabilities(ns string, caps []collection.AtomicCapability) {
	c.capturedNamespace = ns
	c.capturedCaps = caps
}

type capturingRuntime struct {
	name string
	fn   func(AgentRequest, Host) (AgentResult, error)
}

func (r *capturingRuntime) Name() string { return r.name }

func (r *capturingRuntime) Run(_ context.Context, req AgentRequest, host Host) (AgentResult, error) {
	return r.fn(req, host)
}

func TestRunInstallsCapabilitiesFromManifest(t *testing.T) {
	wantCaps := []collection.AtomicCapability{
		{Scope: "cluster", APIGroup: "", Resource: "nodes", Verb: "get"},
		{Scope: "cluster", APIGroup: "", Resource: "nodes", Verb: "patch"},
	}
	pkg := collection.ScriptPackageContent{
		Name:       "test-pkg",
		Entrypoint: "main.js",
		Files:      []collection.ScriptFile{{Path: "main.js", Content: []byte(`exports.main = function(ctx) {}`)}},
	}
	b, err := bundle.Build(bundle.BuildInput{
		Packages:              []collection.ScriptPackageContent{pkg},
		EntrypointPackage:     "test-pkg",
		Runtime:               "js",
		EffectiveCapabilities: wantCaps,
	})
	require.NoError(t, err)
	_, url := serveBundleHTTP(t, b)

	fs := afero.NewMemMapFs()
	require.NoError(t, fs.MkdirAll("/var/run/secrets/kubernetes.io/serviceaccount", 0755))
	require.NoError(t, afero.WriteFile(fs, "/var/run/secrets/kubernetes.io/serviceaccount/namespace", []byte("my-target-ns"), 0644))

	capKC := &capturingKubeClient{}
	called := false
	rt := &capturingRuntime{
		name: "js",
		fn: func(_ AgentRequest, host Host) (AgentResult, error) {
			called = true
			assert.Equal(t, "my-target-ns", capKC.capturedNamespace)
			assert.ElementsMatch(t, wantCaps, capKC.capturedCaps)
			// Verify the host's KubeClient is the same instance that received the caps.
			assert.Equal(t, capKC, host.Kube())
			return AgentResult{ExitCode: 0}, nil
		},
	}

	cfg := Config{BundleURL: url, BundleDigest: b.Digest}
	err = Run(context.Background(), slog.Default(), fs, cfg, fakeRuntimeFactory(rt), capKC)
	require.NoError(t, err)
	assert.True(t, called, "runtime should have been invoked")
}
