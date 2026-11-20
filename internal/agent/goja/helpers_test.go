package goja

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

type mockHost struct {
	mu   sync.Mutex
	logs []string
	kc   agent.KubeClient
}

func (h *mockHost) Log(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, msg)
}

func (h *mockHost) Kube() agent.KubeClient { return h.kc }

func (h *mockHost) getLogs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	dst := make([]string, len(h.logs))
	copy(dst, h.logs)
	return dst
}

func newMockHost() *mockHost {
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	return &mockHost{kc: kube.NewClient(dynClient, mapper, kube.NewPolicy("", nil))}
}

// testKubePolicy returns a permissive allowlist for the goja kube tests
// (configmaps + pods in namespace "default") — capabilities are
// authoritative, so tests explicitly declare what they need.
func testKubePolicy() *kube.Policy {
	return kube.NewPolicy("default", []collection.AtomicCapability{
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "list"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "watch"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "create"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "update"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "patch"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps", Verb: "delete"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "configmaps/status", Verb: "patch"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "get"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "list"},
		{Scope: v1alpha1.ScopeTargetNamespace, APIGroup: "", Resource: "pods", Verb: "watch"},
	})
}

func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		full := filepath.Join(dir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0644))
	}
	return dir
}

func runScript(t *testing.T, files map[string]string, entrypoint string) (agent.AgentResult, error) {
	t.Helper()
	dir := writeBundle(t, files)
	req := agent.AgentRequest{
		BundleDir:  dir,
		Entrypoint: entrypoint,
		Values:     map[string]any{"key": "value", "count": float64(3)},
		Pod: agent.PodReference{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       types.UID("uid-123"),
			NodeName:  "node-1",
		},
	}
	return New(5*time.Second).Run(context.Background(), req, newMockHost())
}
