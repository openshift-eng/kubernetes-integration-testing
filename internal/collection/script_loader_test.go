package collection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type mapResolver map[string]*v1alpha1.ResourceCollection

func (m mapResolver) Resolve(_ context.Context, name string) (*v1alpha1.ResourceCollection, error) {
	rc, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("collection %q not found", name)
	}
	return rc, nil
}

func testCollection(name string, scripts []v1alpha1.ScriptPackage, deps ...string) *v1alpha1.ResourceCollection {
	return &v1alpha1.ResourceCollection{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ResourceCollectionSpec{
			Scripts:           scripts,
			ScriptCollections: deps,
		},
	}
}

func inlineScript(name, content string) v1alpha1.ScriptPackage {
	return v1alpha1.ScriptPackage{Name: name, Inline: content}
}

func TestScriptLoaderLoad(t *testing.T) {
	loader := NewScriptLoader("")

	t.Run("single inline package", func(t *testing.T) {
		resolver := mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScript("my-pkg", "main code"),
			}),
		}

		result, err := loader.Load(context.Background(), "coll", resolver)
		require.NoError(t, err)
		require.Len(t, result.Packages, 1)
		assert.Equal(t, "my-pkg", result.Packages[0].Name)
		assert.Equal(t, InlineEntrypoint, result.Packages[0].Entrypoint)
		assert.NotEmpty(t, result.ContentHash)
	})

	t.Run("direct dependencies", func(t *testing.T) {
		resolver := mapResolver{
			"main": testCollection("main", []v1alpha1.ScriptPackage{
				inlineScript("app", "main"),
			}, "lib"),
			"lib": testCollection("lib", []v1alpha1.ScriptPackage{
				inlineScript("util", "helper"),
			}),
		}

		result, err := loader.Load(context.Background(), "main", resolver)
		require.NoError(t, err)
		assert.Len(t, result.Packages, 2)
		assert.True(t, result.LocalPackages["app"])
		assert.False(t, result.LocalPackages["util"])
	})

	t.Run("transitive dependencies", func(t *testing.T) {
		resolver := mapResolver{
			"a": testCollection("a", []v1alpha1.ScriptPackage{
				inlineScript("pkg-a", "a"),
			}, "b"),
			"b": testCollection("b", []v1alpha1.ScriptPackage{
				inlineScript("pkg-b", "b"),
			}, "c"),
			"c": testCollection("c", []v1alpha1.ScriptPackage{
				inlineScript("pkg-c", "c"),
			}),
		}

		result, err := loader.Load(context.Background(), "a", resolver)
		require.NoError(t, err)
		assert.Len(t, result.Packages, 3)
		assert.True(t, result.LocalPackages["pkg-a"])
		assert.False(t, result.LocalPackages["pkg-b"])
		assert.False(t, result.LocalPackages["pkg-c"])
	})

	t.Run("diamond dependency", func(t *testing.T) {
		resolver := mapResolver{
			"root": testCollection("root", []v1alpha1.ScriptPackage{
				inlineScript("app", "main"),
			}, "left", "right"),
			"left": testCollection("left", []v1alpha1.ScriptPackage{
				inlineScript("left-pkg", "l"),
			}, "shared"),
			"right": testCollection("right", []v1alpha1.ScriptPackage{
				inlineScript("right-pkg", "r"),
			}, "shared"),
			"shared": testCollection("shared", []v1alpha1.ScriptPackage{
				inlineScript("common", "shared-code"),
			}),
		}

		result, err := loader.Load(context.Background(), "root", resolver)
		require.NoError(t, err)
		assert.Len(t, result.Packages, 4)
		assert.True(t, result.LocalPackages["app"])
		assert.False(t, result.LocalPackages["common"])
	})
}

func TestScriptLoaderValidation(t *testing.T) {
	loader := NewScriptLoader("")

	t.Run("missing collection", func(t *testing.T) {
		_, err := loader.Load(context.Background(), "nonexistent", mapResolver{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolving collection")
	})

	t.Run("missing dependency", func(t *testing.T) {
		resolver := mapResolver{
			"main": testCollection("main", []v1alpha1.ScriptPackage{
				inlineScript("app", "code"),
			}, "missing"),
		}

		_, err := loader.Load(context.Background(), "main", resolver)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolving referenced collection")
	})

	t.Run("duplicate package names", func(t *testing.T) {
		resolver := mapResolver{
			"main": testCollection("main", []v1alpha1.ScriptPackage{
				inlineScript("dup", "v1"),
			}, "other"),
			"other": testCollection("other", []v1alpha1.ScriptPackage{
				inlineScript("dup", "v2"),
			}),
		}

		_, err := loader.Load(context.Background(), "main", resolver)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate script package")
		assert.Contains(t, err.Error(), "main")
		assert.Contains(t, err.Error(), "other")
	})

	t.Run("direct cycle", func(t *testing.T) {
		resolver := mapResolver{
			"a": testCollection("a", []v1alpha1.ScriptPackage{
				inlineScript("pkg-a", "a"),
			}, "b"),
			"b": testCollection("b", []v1alpha1.ScriptPackage{
				inlineScript("pkg-b", "b"),
			}, "a"),
		}

		_, err := loader.Load(context.Background(), "a", resolver)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
	})

	t.Run("transitive cycle", func(t *testing.T) {
		resolver := mapResolver{
			"a": testCollection("a", []v1alpha1.ScriptPackage{
				inlineScript("pkg-a", "a"),
			}, "b"),
			"b": testCollection("b", []v1alpha1.ScriptPackage{
				inlineScript("pkg-b", "b"),
			}, "c"),
			"c": testCollection("c", []v1alpha1.ScriptPackage{
				inlineScript("pkg-c", "c"),
			}, "a"),
		}

		_, err := loader.Load(context.Background(), "a", resolver)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
	})

	t.Run("nil FS with root package", func(t *testing.T) {
		resolver := mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				{Name: "pkg", Root: "scripts/dir", Entrypoint: "main.js"},
			}),
		}

		_, err := loader.Load(context.Background(), "coll", resolver)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires a collection source directory")
	})
}

func TestScriptLoaderSourceResolution(t *testing.T) {
	t.Run("user overrides builtin", func(t *testing.T) {
		dir := t.TempDir()
		collDir := filepath.Join(dir, "openshift-mco", "scripts", "mcd-agent")
		require.NoError(t, os.MkdirAll(collDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(collDir, "main.js"), []byte("user-version"), 0644))

		loader := NewScriptLoader(dir)
		resolver := mapResolver{
			"mco": {
				ObjectMeta: metav1.ObjectMeta{Name: "mco"},
				Spec: v1alpha1.ResourceCollectionSpec{
					Source: "openshift-mco",
					Scripts: []v1alpha1.ScriptPackage{
						{Name: "mcd-agent", Root: "scripts/mcd-agent", Entrypoint: "main.js"},
					},
				},
			},
		}

		result, err := loader.Load(context.Background(), "mco", resolver)
		require.NoError(t, err)
		require.Len(t, result.Packages, 1)
		assert.Equal(t, []byte("user-version"), result.Packages[0].Files[0].Content)
	})

	t.Run("missing source returns ErrSourceMissing", func(t *testing.T) {
		loader := NewScriptLoader(t.TempDir())
		resolver := mapResolver{
			"ghost": {
				ObjectMeta: metav1.ObjectMeta{Name: "ghost"},
				Spec: v1alpha1.ResourceCollectionSpec{
					Source: "nonexistent-collection",
					Scripts: []v1alpha1.ScriptPackage{
						{Name: "pkg", Inline: "code"},
					},
				},
			},
		}

		_, err := loader.Load(context.Background(), "ghost", resolver)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSourceMissing)
	})

	t.Run("missing dependency source returns ErrSourceMissing", func(t *testing.T) {
		loader := NewScriptLoader(t.TempDir())
		resolver := mapResolver{
			"root": {
				ObjectMeta: metav1.ObjectMeta{Name: "root"},
				Spec: v1alpha1.ResourceCollectionSpec{
					Scripts:           []v1alpha1.ScriptPackage{{Name: "pkg", Inline: "code"}},
					ScriptCollections: []string{"dep"},
				},
			},
			"dep": {
				ObjectMeta: metav1.ObjectMeta{Name: "dep"},
				Spec: v1alpha1.ResourceCollectionSpec{
					Source: "nonexistent-dep",
					Scripts: []v1alpha1.ScriptPackage{
						{Name: "lib", Inline: "code"},
					},
				},
			},
		}

		_, err := loader.Load(context.Background(), "root", resolver)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSourceMissing)
	})

	t.Run("falls back to builtin", func(t *testing.T) {
		loader := NewScriptLoader(t.TempDir())
		resolver := mapResolver{
			"mco": {
				ObjectMeta: metav1.ObjectMeta{Name: "mco"},
				Spec: v1alpha1.ResourceCollectionSpec{
					Source: "openshift-mco",
					Scripts: []v1alpha1.ScriptPackage{
						{Name: "mcd-agent", Root: "scripts/mcd-agent", Entrypoint: "main.js"},
					},
				},
			},
		}

		result, err := loader.Load(context.Background(), "mco", resolver)
		require.NoError(t, err)
		require.Len(t, result.Packages, 1)
		assert.NotEmpty(t, result.Packages[0].Files)
	})
}

func TestScriptLoaderContentHash(t *testing.T) {
	loader := NewScriptLoader("")

	t.Run("same effective content produces same hash", func(t *testing.T) {
		r1, err := loader.Load(context.Background(), "coll-a", mapResolver{
			"coll-a": testCollection("coll-a", []v1alpha1.ScriptPackage{
				inlineScript("app", "code"),
			}, "dep-a"),
			"dep-a": testCollection("dep-a", []v1alpha1.ScriptPackage{
				inlineScript("lib", "helper"),
			}),
		})
		require.NoError(t, err)

		r2, err := loader.Load(context.Background(), "coll-b", mapResolver{
			"coll-b": testCollection("coll-b", []v1alpha1.ScriptPackage{
				inlineScript("app", "code"),
			}, "dep-b"),
			"dep-b": testCollection("dep-b", []v1alpha1.ScriptPackage{
				inlineScript("lib", "helper"),
			}),
		})
		require.NoError(t, err)

		assert.Equal(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("changes when root content changes", func(t *testing.T) {
		mk := func(content string) mapResolver {
			return mapResolver{
				"coll": testCollection("coll", []v1alpha1.ScriptPackage{
					inlineScript("pkg", content),
				}),
			}
		}

		r1, err := loader.Load(context.Background(), "coll", mk("v1"))
		require.NoError(t, err)

		r2, err := loader.Load(context.Background(), "coll", mk("v2"))
		require.NoError(t, err)

		assert.NotEqual(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("changes when dependency content changes", func(t *testing.T) {
		mk := func(depContent string) mapResolver {
			return mapResolver{
				"main": testCollection("main", []v1alpha1.ScriptPackage{
					inlineScript("app", "stable"),
				}, "lib"),
				"lib": testCollection("lib", []v1alpha1.ScriptPackage{
					inlineScript("util", depContent),
				}),
			}
		}

		r1, err := loader.Load(context.Background(), "main", mk("v1"))
		require.NoError(t, err)

		r2, err := loader.Load(context.Background(), "main", mk("v2"))
		require.NoError(t, err)

		assert.NotEqual(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("changes when transitive dependency content changes", func(t *testing.T) {
		mk := func(grandchildContent string) mapResolver {
			return mapResolver{
				"root": testCollection("root", []v1alpha1.ScriptPackage{
					inlineScript("app", "stable"),
				}, "mid"),
				"mid": testCollection("mid", []v1alpha1.ScriptPackage{
					inlineScript("mid-pkg", "stable"),
				}, "leaf"),
				"leaf": testCollection("leaf", []v1alpha1.ScriptPackage{
					inlineScript("leaf-pkg", grandchildContent),
				}),
			}
		}

		r1, err := loader.Load(context.Background(), "root", mk("v1"))
		require.NoError(t, err)

		r2, err := loader.Load(context.Background(), "root", mk("v2"))
		require.NoError(t, err)

		assert.NotEqual(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("same content direct vs transitive produces same hash", func(t *testing.T) {
		r1, err := loader.Load(context.Background(), "flat", mapResolver{
			"flat": testCollection("flat", []v1alpha1.ScriptPackage{
				inlineScript("app", "main"),
			}, "dep"),
			"dep": testCollection("dep", []v1alpha1.ScriptPackage{
				inlineScript("lib", "helper"),
			}),
		})
		require.NoError(t, err)

		r2, err := loader.Load(context.Background(), "deep", mapResolver{
			"deep": testCollection("deep", []v1alpha1.ScriptPackage{
				inlineScript("app", "main"),
			}, "mid"),
			"mid": testCollection("mid", nil, "leaf"),
			"leaf": testCollection("leaf", []v1alpha1.ScriptPackage{
				inlineScript("lib", "helper"),
			}),
		})
		require.NoError(t, err)

		assert.Equal(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("changes when package moves from root to dependency", func(t *testing.T) {
		r1, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScript("app", "main"),
				inlineScript("lib", "helper"),
			}),
		})
		require.NoError(t, err)

		r2, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScript("app", "main"),
			}, "dep"),
			"dep": testCollection("dep", []v1alpha1.ScriptPackage{
				inlineScript("lib", "helper"),
			}),
		})
		require.NoError(t, err)

		assert.NotEqual(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("dependency with no scripts produces EmptyHash", func(t *testing.T) {
		result, err := loader.Load(context.Background(), "root", mapResolver{
			"root":      testCollection("root", nil, "empty-dep"),
			"empty-dep": testCollection("empty-dep", nil),
		})
		require.NoError(t, err)
		assert.Equal(t, EmptyHash, result.ContentHash)
	})
}

func TestScriptLoaderContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resolver := mapResolver{
		"coll": testCollection("coll", []v1alpha1.ScriptPackage{
			inlineScript("app", "code"),
		}, "dep"),
	}

	cancellingResolver := &contextCheckResolver{inner: resolver}
	loader := NewScriptLoader("")

	_, err := loader.Load(ctx, "coll", cancellingResolver)
	require.Error(t, err)
}

type contextCheckResolver struct {
	inner mapResolver
}

func (r *contextCheckResolver) Resolve(ctx context.Context, name string) (*v1alpha1.ResourceCollection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.inner.Resolve(ctx, name)
}

func inlineScriptWithCaps(name, content string, caps ...v1alpha1.ScriptCapability) v1alpha1.ScriptPackage {
	return v1alpha1.ScriptPackage{Name: name, Inline: content, Capabilities: caps}
}

func cap(scope v1alpha1.ScriptCapabilityScope, apiGroup, resource string, verbs ...string) v1alpha1.ScriptCapability {
	return v1alpha1.ScriptCapability{
		Scope: scope,
		PolicyRule: rbacv1.PolicyRule{
			APIGroups: []string{apiGroup},
			Resources: []string{resource},
			Verbs:     verbs,
		},
	}
}

func TestScriptLoaderEffectiveCapabilities(t *testing.T) {
	loader := NewScriptLoader("")

	t.Run("single package capabilities", func(t *testing.T) {
		resolver := mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "code",
					cap(v1alpha1.ScopeCluster, "", "nodes", "get", "list"),
				),
			}),
		}

		result, err := loader.Load(context.Background(), "coll", resolver)
		require.NoError(t, err)
		require.Len(t, result.EffectiveCapabilities, 2)
		assert.Equal(t, AtomicCapability{Scope: v1alpha1.ScopeCluster, APIGroup: "", Resource: "nodes", Verb: "get"}, result.EffectiveCapabilities[0])
		assert.Equal(t, AtomicCapability{Scope: v1alpha1.ScopeCluster, APIGroup: "", Resource: "nodes", Verb: "list"}, result.EffectiveCapabilities[1])
	})

	t.Run("expands cartesian product", func(t *testing.T) {
		resolver := mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "code", v1alpha1.ScriptCapability{
					Scope: v1alpha1.ScopeTargetNamespace,
					PolicyRule: rbacv1.PolicyRule{
						APIGroups: []string{"", "apps"},
						Resources: []string{"pods", "deployments"},
						Verbs:     []string{"get"},
					},
				}),
			}),
		}

		result, err := loader.Load(context.Background(), "coll", resolver)
		require.NoError(t, err)
		assert.Len(t, result.EffectiveCapabilities, 4)
	})

	t.Run("deduplicates across packages", func(t *testing.T) {
		resolver := mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("pkg-a", "a",
					cap(v1alpha1.ScopeCluster, "", "nodes", "get"),
				),
				inlineScriptWithCaps("pkg-b", "b",
					cap(v1alpha1.ScopeCluster, "", "nodes", "get"),
				),
			}),
		}

		result, err := loader.Load(context.Background(), "coll", resolver)
		require.NoError(t, err)
		assert.Len(t, result.EffectiveCapabilities, 1)
	})

	t.Run("merges across dependencies", func(t *testing.T) {
		resolver := mapResolver{
			"main": testCollection("main", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "main",
					cap(v1alpha1.ScopeCluster, "", "nodes", "get"),
				),
			}, "lib"),
			"lib": testCollection("lib", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("util", "helper",
					cap(v1alpha1.ScopeCluster, "", "nodes", "list"),
				),
			}),
		}

		result, err := loader.Load(context.Background(), "main", resolver)
		require.NoError(t, err)
		assert.Len(t, result.EffectiveCapabilities, 2)
	})

	t.Run("no capabilities produces empty set", func(t *testing.T) {
		resolver := mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScript("app", "code"),
			}),
		}

		result, err := loader.Load(context.Background(), "coll", resolver)
		require.NoError(t, err)
		assert.Empty(t, result.EffectiveCapabilities)
	})

	t.Run("sorted deterministically", func(t *testing.T) {
		resolver := mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "code",
					cap(v1alpha1.ScopeCluster, "apps", "deployments", "get"),
					cap(v1alpha1.ScopeTargetNamespace, "", "pods", "list"),
					cap(v1alpha1.ScopeCluster, "", "nodes", "watch"),
				),
			}),
		}

		result, err := loader.Load(context.Background(), "coll", resolver)
		require.NoError(t, err)
		require.Len(t, result.EffectiveCapabilities, 3)
		// scope: cluster < targetNamespace; within cluster: "" < "apps"
		assert.Equal(t, v1alpha1.ScopeCluster, result.EffectiveCapabilities[0].Scope)
		assert.Equal(t, "", result.EffectiveCapabilities[0].APIGroup)
		assert.Equal(t, v1alpha1.ScopeCluster, result.EffectiveCapabilities[1].Scope)
		assert.Equal(t, "apps", result.EffectiveCapabilities[1].APIGroup)
		assert.Equal(t, v1alpha1.ScopeTargetNamespace, result.EffectiveCapabilities[2].Scope)
	})
}

func TestScriptLoaderCapabilityHash(t *testing.T) {
	loader := NewScriptLoader("")

	t.Run("changes when capabilities added", func(t *testing.T) {
		r1, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScript("app", "code"),
			}),
		})
		require.NoError(t, err)

		r2, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "code",
					cap(v1alpha1.ScopeCluster, "", "nodes", "get"),
				),
			}),
		})
		require.NoError(t, err)

		assert.NotEqual(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("same effective set from different groupings", func(t *testing.T) {
		// Two apiGroups+resources in one rule
		r1, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "code", v1alpha1.ScriptCapability{
					Scope: v1alpha1.ScopeTargetNamespace,
					PolicyRule: rbacv1.PolicyRule{
						APIGroups: []string{"", "apps"},
						Resources: []string{"pods", "deployments"},
						Verbs:     []string{"get"},
					},
				}),
			}),
		})
		require.NoError(t, err)

		// Same permissions split into separate rules
		r2, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "code",
					cap(v1alpha1.ScopeTargetNamespace, "", "pods", "get"),
					cap(v1alpha1.ScopeTargetNamespace, "", "deployments", "get"),
					cap(v1alpha1.ScopeTargetNamespace, "apps", "pods", "get"),
					cap(v1alpha1.ScopeTargetNamespace, "apps", "deployments", "get"),
				),
			}),
		})
		require.NoError(t, err)

		assert.Equal(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("moving capability between packages preserves hash", func(t *testing.T) {
		nodeCap := cap(v1alpha1.ScopeCluster, "", "nodes", "get")
		podCap := cap(v1alpha1.ScopeTargetNamespace, "", "pods", "list")

		// Both caps in pkg-a
		r1, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("pkg-a", "a", nodeCap, podCap),
				inlineScript("pkg-b", "b"),
			}),
		})
		require.NoError(t, err)

		// Split caps across packages
		r2, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("pkg-a", "a", nodeCap),
				inlineScriptWithCaps("pkg-b", "b", podCap),
			}),
		})
		require.NoError(t, err)

		assert.Equal(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("moving capability to dependency preserves hash", func(t *testing.T) {
		nodeCap := cap(v1alpha1.ScopeCluster, "", "nodes", "get")

		// Cap in local package
		r1, err := loader.Load(context.Background(), "main", mapResolver{
			"main": testCollection("main", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "main", nodeCap),
			}, "lib"),
			"lib": testCollection("lib", []v1alpha1.ScriptPackage{
				inlineScript("util", "helper"),
			}),
		})
		require.NoError(t, err)

		// Same cap in dependency
		r2, err := loader.Load(context.Background(), "main", mapResolver{
			"main": testCollection("main", []v1alpha1.ScriptPackage{
				inlineScript("app", "main"),
			}, "lib"),
			"lib": testCollection("lib", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("util", "helper", nodeCap),
			}),
		})
		require.NoError(t, err)

		assert.Equal(t, r1.ContentHash, r2.ContentHash)
	})

	t.Run("absent and empty capabilities produce same hash", func(t *testing.T) {
		r1, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScript("app", "code"),
			}),
		})
		require.NoError(t, err)

		r2, err := loader.Load(context.Background(), "coll", mapResolver{
			"coll": testCollection("coll", []v1alpha1.ScriptPackage{
				inlineScriptWithCaps("app", "code"),
			}),
		})
		require.NoError(t, err)

		assert.Equal(t, r1.ContentHash, r2.ContentHash)
	})
}
