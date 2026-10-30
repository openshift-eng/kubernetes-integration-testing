package cel

import (
	"encoding/json"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

type Context struct {
	Cluster ClusterContext
	Values  map[string]any
	Release []map[string]interface{}
}

type ClusterContext struct {
	Platform     string
	APIServerURL string
	FeatureSet   string
	Version      string
	Image        string
}

type StatusResult struct {
	Kind   string
	Name   string
	Status map[string]interface{}
}

type StatusPatchInput struct {
	Kind   string
	Name   string
	Status apiextensionsv1.JSON
}

func EvalStatusPatches(ctx Context, patches []StatusPatchInput) ([]StatusResult, error) {
	env, err := newEnv()
	if err != nil {
		return nil, fmt.Errorf("creating CEL environment: %w", err)
	}

	act := activation(ctx)
	var results []StatusResult
	for _, p := range patches {
		status, err := evalPatch(env, act, p.Status)
		if err != nil {
			return nil, fmt.Errorf("evaluating status patch for %s/%s: %w", p.Kind, p.Name, err)
		}
		results = append(results, StatusResult{Kind: p.Kind, Name: p.Name, Status: status})
	}
	return results, nil
}

func newEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("cluster", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("values", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("release", cel.ListType(cel.DynType)),
		celMergeFunc(),
		celAnnotationFunc(),
		celFindReleaseFunc(),
		celFindReleaseByProfileFunc(),
	)
}

func activation(ctx Context) map[string]interface{} {
	release := make([]interface{}, len(ctx.Release))
	for i, m := range ctx.Release {
		release[i] = m
	}
	values := make(map[string]interface{}, len(ctx.Values))
	for k, v := range ctx.Values {
		values[k] = v
	}
	return map[string]interface{}{
		"cluster": map[string]interface{}{
			"platform":     ctx.Cluster.Platform,
			"apiServerURL": ctx.Cluster.APIServerURL,
			"featureSet":   ctx.Cluster.FeatureSet,
			"version":      ctx.Cluster.Version,
			"image":        ctx.Cluster.Image,
		},
		"values":  values,
		"release": release,
	}
}

func evalPatch(env *cel.Env, act map[string]interface{}, raw apiextensionsv1.JSON) (map[string]interface{}, error) {
	var parsed interface{}
	if err := json.Unmarshal(raw.Raw, &parsed); err != nil {
		return nil, fmt.Errorf("parsing status patch JSON: %w", err)
	}

	switch v := parsed.(type) {
	case string:
		return evalExpr(env, act, v)
	case map[string]interface{}:
		return evalMap(env, act, v)
	default:
		return nil, fmt.Errorf("status patch must be a CEL expression string or a map, got %T", parsed)
	}
}

func evalExpr(env *cel.Env, act map[string]interface{}, expr string) (map[string]interface{}, error) {
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compiling CEL: %w", issues.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("creating CEL program: %w", err)
	}

	out, _, err := prg.Eval(act)
	if err != nil {
		return nil, fmt.Errorf("evaluating CEL: %w", err)
	}

	return refValToMap(out)
}

func evalMap(env *cel.Env, act map[string]interface{}, m map[string]interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		resolved, err := evalValue(env, act, v)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", k, err)
		}
		result[k] = resolved
	}
	return result, nil
}

func evalValue(env *cel.Env, act map[string]interface{}, v interface{}) (interface{}, error) {
	switch val := v.(type) {
	case string:
		ast, issues := env.Compile(val)
		if issues != nil && issues.Err() != nil {
			return val, nil
		}
		prg, err := env.Program(ast)
		if err != nil {
			return val, nil
		}
		out, _, err := prg.Eval(act)
		if err != nil {
			return val, nil
		}
		return unwrapRefVal(out), nil
	case map[string]interface{}:
		return evalMap(env, act, val)
	default:
		return v, nil
	}
}

func refValToMap(val ref.Val) (map[string]interface{}, error) {
	native := val.Value()
	switch v := native.(type) {
	case map[string]interface{}:
		return v, nil
	case map[ref.Val]ref.Val:
		result := make(map[string]interface{}, len(v))
		for mk, mv := range v {
			key, ok := mk.Value().(string)
			if !ok {
				return nil, fmt.Errorf("map key must be string, got %T", mk.Value())
			}
			result[key] = unwrapRefVal(mv)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("CEL expression must return a map, got %T", native)
	}
}

func celMergeFunc() cel.EnvOption {
	return cel.Function("merge",
		cel.MemberOverload("map_merge_map",
			[]*cel.Type{cel.MapType(cel.StringType, cel.DynType), cel.MapType(cel.StringType, cel.DynType)},
			cel.MapType(cel.StringType, cel.DynType),
			cel.BinaryBinding(func(lhs, rhs ref.Val) ref.Val {
				baseMap, err := refValToNative(lhs)
				if err != nil {
					return types.NewErr("merge: %s", err)
				}
				overlayMap, err := refValToNative(rhs)
				if err != nil {
					return types.NewErr("merge: %s", err)
				}
				merged := deepMerge(baseMap, overlayMap)
				return types.DefaultTypeAdapter.NativeToValue(merged)
			}),
		),
	)
}

// annotation(obj, key) returns the annotation value or "" if missing.
func celAnnotationFunc() cel.EnvOption {
	return cel.Function("annotation",
		cel.Overload("annotation_map_string",
			[]*cel.Type{cel.MapType(cel.StringType, cel.DynType), cel.StringType},
			cel.StringType,
			cel.BinaryBinding(func(obj, key ref.Val) ref.Val {
				m, err := refValToNative(obj)
				if err != nil {
					return types.String("")
				}
				metadata, _ := m["metadata"].(map[string]interface{})
				if metadata == nil {
					return types.String("")
				}
				annotations, _ := metadata["annotations"].(map[string]interface{})
				if annotations == nil {
					return types.String("")
				}
				v, _ := annotations[string(key.(types.String))].(string)
				return types.String(v)
			}),
		),
	)
}

// findRelease(release, kind, name) returns the first manifest matching kind+name.
func celFindReleaseFunc() cel.EnvOption {
	return cel.Function("findRelease",
		cel.Overload("findRelease_list_string_string",
			[]*cel.Type{cel.ListType(cel.DynType), cel.StringType, cel.StringType},
			cel.DynType,
			cel.FunctionBinding(func(args ...ref.Val) ref.Val {
				kind := string(args[1].(types.String))
				name := string(args[2].(types.String))
				m := findInRelease(args[0], func(m map[string]interface{}, meta map[string]interface{}) bool {
					return m["kind"] == kind && meta["name"] == name
				})
				if m == nil {
					return types.NewErr("findRelease: %s/%s not found", kind, name)
				}
				return types.DefaultTypeAdapter.NativeToValue(m)
			}),
		),
	)
}

// findReleaseByProfile(release, kind, name, featureSet, profile) returns the first
// manifest matching kind+name with the given feature-set and profile annotations.
func celFindReleaseByProfileFunc() cel.EnvOption {
	return cel.Function("findReleaseByProfile",
		cel.Overload("findReleaseByProfile_list_string_string_string_string",
			[]*cel.Type{cel.ListType(cel.DynType), cel.StringType, cel.StringType, cel.StringType, cel.StringType},
			cel.DynType,
			cel.FunctionBinding(func(args ...ref.Val) ref.Val {
				kind := string(args[1].(types.String))
				name := string(args[2].(types.String))
				featureSet := string(args[3].(types.String))
				profile := string(args[4].(types.String))
				profileKey := "include.release.openshift.io/" + profile

				m := findInRelease(args[0], func(m map[string]interface{}, meta map[string]interface{}) bool {
					if m["kind"] != kind || meta["name"] != name {
						return false
					}
					annotations, _ := meta["annotations"].(map[string]interface{})
					if annotations == nil {
						return false
					}
					fs, _ := annotations["release.openshift.io/feature-set"].(string)
					_, hasProfile := annotations[profileKey]
					return fs == featureSet && hasProfile
				})
				if m == nil {
					return types.NewErr("findReleaseByProfile: %s/%s not found for featureSet=%s profile=%s", kind, name, featureSet, profile)
				}
				return types.DefaultTypeAdapter.NativeToValue(m)
			}),
		),
	)
}

func findInRelease(list ref.Val, match func(obj map[string]interface{}, metadata map[string]interface{}) bool) map[string]interface{} {
	it := list.Value()
	var items []interface{}
	switch v := it.(type) {
	case []interface{}:
		items = v
	case []ref.Val:
		for _, rv := range v {
			items = append(items, rv.Value())
		}
	default:
		return nil
	}

	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		metadata, _ := m["metadata"].(map[string]interface{})
		if metadata == nil {
			continue
		}
		if match(m, metadata) {
			return m
		}
	}
	return nil
}

func refValToNative(val ref.Val) (map[string]interface{}, error) {
	native := val.Value()
	switch v := native.(type) {
	case map[string]interface{}:
		return v, nil
	case map[ref.Val]ref.Val:
		result := make(map[string]interface{}, len(v))
		for mk, mv := range v {
			key, ok := mk.Value().(string)
			if !ok {
				return nil, fmt.Errorf("map key must be string, got %T", mk.Value())
			}
			result[key] = unwrapRefVal(mv)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("expected map, got %T", native)
	}
}

func deepMerge(base, overlay map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overlay {
		if bv, ok := result[k]; ok {
			bMap, bOk := bv.(map[string]interface{})
			oMap, oOk := v.(map[string]interface{})
			if bOk && oOk {
				result[k] = deepMerge(bMap, oMap)
				continue
			}
		}
		result[k] = v
	}
	return result
}

func unwrapRefVal(val ref.Val) interface{} {
	native := val.Value()
	switch v := native.(type) {
	case map[ref.Val]ref.Val:
		result := make(map[string]interface{}, len(v))
		for mk, mv := range v {
			if key, ok := mk.Value().(string); ok {
				result[key] = unwrapRefVal(mv)
			}
		}
		return result
	case []ref.Val:
		result := make([]interface{}, len(v))
		for i, item := range v {
			result[i] = unwrapRefVal(item)
		}
		return result
	default:
		return native
	}
}
