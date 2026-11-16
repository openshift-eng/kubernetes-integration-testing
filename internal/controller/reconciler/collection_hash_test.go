package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeLoader struct {
	hash string
	err  error
}

func (f *fakeLoader) Load(_ context.Context, _ string, _ collection.ScriptCollectionResolver) (*collection.LoadResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &collection.LoadResult{ContentHash: f.hash}, nil
}

func hashScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

func TestComputeContentHash(t *testing.T) {
	renderHash := collection.EmptyHash

	t.Run("no scripts no patches", func(t *testing.T) {
		r := &collectionReconciler{scriptLoader: &fakeLoader{}}
		rc := &v1alpha1.ResourceCollection{}

		h, err := r.computeContentHash(context.Background(), rc, renderHash)
		require.NoError(t, err)
		assert.Len(t, h, 64)
	})

	t.Run("with script hash", func(t *testing.T) {
		scriptHash := "aa" + collection.EmptyHash[2:]
		r := &collectionReconciler{scriptLoader: &fakeLoader{hash: scriptHash}}
		rc := &v1alpha1.ResourceCollection{
			Spec: v1alpha1.ResourceCollectionSpec{
				Scripts: []v1alpha1.ScriptPackage{{Name: "pkg", Inline: "code"}},
			},
		}

		h1, err := r.computeContentHash(context.Background(), rc, renderHash)
		require.NoError(t, err)

		r2 := &collectionReconciler{scriptLoader: &fakeLoader{}}
		h2, err := r2.computeContentHash(context.Background(), &v1alpha1.ResourceCollection{}, renderHash)
		require.NoError(t, err)

		assert.NotEqual(t, h1, h2)
	})

	t.Run("with status patches", func(t *testing.T) {
		r := &collectionReconciler{scriptLoader: &fakeLoader{}}
		rc := &v1alpha1.ResourceCollection{
			Spec: v1alpha1.ResourceCollectionSpec{
				StatusPatches: []v1alpha1.StatusPatch{
					{Kind: "Node", Name: "n", Status: apiextensionsv1.JSON{Raw: []byte(`{"ready":true}`)}},
				},
			},
		}

		h1, err := r.computeContentHash(context.Background(), rc, renderHash)
		require.NoError(t, err)

		h2, err := r.computeContentHash(context.Background(), &v1alpha1.ResourceCollection{}, renderHash)
		require.NoError(t, err)

		assert.NotEqual(t, h1, h2)
	})

	t.Run("loader error propagates", func(t *testing.T) {
		r := &collectionReconciler{scriptLoader: &fakeLoader{err: fmt.Errorf("boom")}}
		rc := &v1alpha1.ResourceCollection{
			Spec: v1alpha1.ResourceCollectionSpec{
				Scripts: []v1alpha1.ScriptPackage{{Name: "pkg", Inline: "code"}},
			},
		}

		_, err := r.computeContentHash(context.Background(), rc, renderHash)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "boom")
	})
}

func TestCollectionScriptMapping(t *testing.T) {
	mkCollection := func(name string, deps ...string) *v1alpha1.ResourceCollection {
		return &v1alpha1.ResourceCollection{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.ResourceCollectionSpec{
				ScriptCollections: deps,
			},
		}
	}

	t.Run("change in leaf enqueues mid and root", func(t *testing.T) {
		root := mkCollection("root", "mid")
		mid := mkCollection("mid", "leaf")
		leaf := mkCollection("leaf")

		c := fake.NewClientBuilder().
			WithScheme(hashScheme(t)).
			WithObjects(root, mid, leaf).
			Build()
		r := &collectionReconciler{
			client: c,
			log:    slog.New(slog.DiscardHandler),
		}

		requests := r.collectionScriptMapping(context.Background(), leaf)
		names := requestNames(requests)
		assert.Contains(t, names, "mid")
		assert.Contains(t, names, "root")
		assert.NotContains(t, names, "leaf")
	})

	t.Run("change in root enqueues its dependencies", func(t *testing.T) {
		root := mkCollection("root", "dep-a", "dep-b")
		depA := mkCollection("dep-a")
		depB := mkCollection("dep-b")

		c := fake.NewClientBuilder().
			WithScheme(hashScheme(t)).
			WithObjects(root, depA, depB).
			Build()
		r := &collectionReconciler{
			client: c,
			log:    slog.New(slog.DiscardHandler),
		}

		requests := r.collectionScriptMapping(context.Background(), root)
		names := requestNames(requests)
		assert.Contains(t, names, "dep-a")
		assert.Contains(t, names, "dep-b")
	})

	t.Run("diamond deduplicates", func(t *testing.T) {
		root := mkCollection("root", "left", "right")
		left := mkCollection("left", "shared")
		right := mkCollection("right", "shared")
		shared := mkCollection("shared")

		c := fake.NewClientBuilder().
			WithScheme(hashScheme(t)).
			WithObjects(root, left, right, shared).
			Build()
		r := &collectionReconciler{
			client: c,
			log:    slog.New(slog.DiscardHandler),
		}

		requests := r.collectionScriptMapping(context.Background(), shared)
		names := requestNames(requests)
		assert.Contains(t, names, "left")
		assert.Contains(t, names, "right")
		assert.Contains(t, names, "root")
		count := 0
		for _, n := range names {
			if n == "root" {
				count++
			}
		}
		assert.Equal(t, 1, count, "root must appear exactly once")
	})
}

func requestNames(requests []ctrl.Request) []string {
	names := make([]string, len(requests))
	for i, r := range requests {
		names[i] = r.Name
	}
	return names
}

func TestBuildDependents(t *testing.T) {
	collections := []v1alpha1.ResourceCollection{
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: v1alpha1.ResourceCollectionSpec{ScriptCollections: []string{"c"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: v1alpha1.ResourceCollectionSpec{ScriptCollections: []string{"c"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
	}

	deps := buildDependents(collections)
	assert.ElementsMatch(t, []string{"a", "b"}, deps["c"])
	assert.Empty(t, deps["a"])
}
