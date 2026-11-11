package goja

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/kube"
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
	return &mockHost{kc: kube.NewClient(dynClient, mapper, kube.NewPolicy())}
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
	return New().Run(context.Background(), req, newMockHost())
}
