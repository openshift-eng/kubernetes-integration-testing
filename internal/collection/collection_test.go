package collection

import (
	"testing"
	"testing/fstest"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var stubCertFuncMap = template.FuncMap{
	"caCert":     func(name string) (string, error) { return "STUB-CA-CERT", nil },
	"signedCert": func(ca, cn string) (string, error) { return "STUB-CERT", nil },
	"signedKey":  func(ca, cn string) (string, error) { return "STUB-KEY", nil },
}

func TestLoadBuiltin(t *testing.T) {
	coll, err := Load("openshift-default", nil, stubCertFuncMap)
	if err != nil {
		t.Fatal(err)
	}
	if coll.Name() != "openshift-default" {
		t.Errorf("Name() = %q, want openshift-default", coll.Name())
	}
}

func TestLoadUnknown(t *testing.T) {
	_, err := Load("nonexistent", nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown collection")
	}
}

func TestLoadUserCollection(t *testing.T) {
	userFS := fstest.MapFS{
		"my-profile/resource.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  key: value
`)},
	}

	coll, err := Load("my-profile", userFS, nil)
	if err != nil {
		t.Fatal(err)
	}
	if coll.Name() != "my-profile" {
		t.Errorf("Name() = %q, want my-profile", coll.Name())
	}

	result, err := coll.Render(RenderContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(result.Objects))
	}
	if result.Objects[0].GetKind() != "ConfigMap" || result.Objects[0].GetName() != "test" {
		t.Errorf("got %s/%s, want ConfigMap/test", result.Objects[0].GetKind(), result.Objects[0].GetName())
	}
}

func TestUserCollectionOverridesBuiltin(t *testing.T) {
	userFS := fstest.MapFS{
		"openshift-default/custom.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: override
data:
  source: user
`)},
	}

	coll, err := Load("openshift-default", userFS, stubCertFuncMap)
	if err != nil {
		t.Fatal(err)
	}

	result, err := coll.Render(RenderContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Objects) != 1 {
		t.Errorf("got %d objects, want 1 (user override should replace builtin)", len(result.Objects))
	}
}

func TestRenderBuiltinObjects(t *testing.T) {
	coll, err := Load("openshift-default", nil, stubCertFuncMap)
	if err != nil {
		t.Fatal(err)
	}

	result, err := coll.Render(RenderContext{
		FeatureSet:   "TechPreviewNoUpgrade",
		APIServerURL: "https://127.0.0.1:6443",
		Platform:     "None",
		NetworkType:  "OVNKubernetes",
		Values:       map[string]any{"serviceNetwork": []any{"10.96.0.0/16"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// TODO: asserting an exact count is fragile — every new template breaks this.
	// Consider checking for required kinds instead.
	if len(result.Objects) != 13 {
		t.Fatalf("got %d objects, want 13", len(result.Objects))
	}

	expected := []string{"APIServer", "ClusterVersion", "ConfigMap", "DNS", "FeatureGate", "Image", "Infrastructure", "Namespace", "Network", "Proxy"}
	found := make(map[string]bool)
	for _, obj := range result.Objects {
		found[obj.GetKind()] = true
	}
	for _, kind := range expected {
		if !found[kind] {
			t.Errorf("missing expected kind %q", kind)
		}
	}
}

func TestRenderTemplateValues(t *testing.T) {
	coll, err := Load("openshift-default", nil, stubCertFuncMap)
	if err != nil {
		t.Fatal(err)
	}

	ctx := RenderContext{
		FeatureSet:   "TechPreviewNoUpgrade",
		APIServerURL: "https://10.0.0.1:6443",
		Platform:     "AWS",
		NetworkType:  "OpenShiftSDN",
		Values:       map[string]any{"serviceNetwork": []any{"10.128.0.0/14"}},
	}

	result, err := coll.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, obj := range result.Objects {
		switch obj.GetKind() {
		case "FeatureGate":
			got, _, _ := unstructured.NestedString(obj.Object, "spec", "featureSet")
			if got != "TechPreviewNoUpgrade" {
				t.Errorf("FeatureGate featureSet = %q, want TechPreviewNoUpgrade", got)
			}
		case "Infrastructure":
			got, _, _ := unstructured.NestedString(obj.Object, "status", "platform")
			if got != "AWS" {
				t.Errorf("Infrastructure platform = %q, want AWS", got)
			}
			got, _, _ = unstructured.NestedString(obj.Object, "status", "platformStatus", "type")
			if got != "AWS" {
				t.Errorf("Infrastructure platformStatus.type = %q, want AWS", got)
			}
			got, _, _ = unstructured.NestedString(obj.Object, "status", "apiServerInternalURI")
			if got != "https://10.0.0.1:6443" {
				t.Errorf("Infrastructure apiServerInternalURI = %q, want https://10.0.0.1:6443", got)
			}
		case "Network":
			nets, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "serviceNetwork")
			if len(nets) != 1 || nets[0] != "10.128.0.0/14" {
				t.Errorf("Network serviceNetwork = %v, want [10.128.0.0/14]", nets)
			}
			got, _, _ := unstructured.NestedString(obj.Object, "spec", "networkType")
			if got != "OpenShiftSDN" {
				t.Errorf("Network networkType = %q, want OpenShiftSDN", got)
			}
		}
	}
}

func TestRenderUserTemplate(t *testing.T) {
	userFS := fstest.MapFS{
		"templated/resource.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: config
data:
  platform: "{{ .Platform }}"
  networkType: "{{ .NetworkType }}"
`)},
	}

	coll, err := Load("templated", userFS, nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := coll.Render(RenderContext{Platform: "GCP", NetworkType: "Calico"})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(result.Objects))
	}
	data, _, _ := unstructured.NestedStringMap(result.Objects[0].Object, "data")
	if data["platform"] != "GCP" {
		t.Errorf("platform = %q, want GCP", data["platform"])
	}
	if data["networkType"] != "Calico" {
		t.Errorf("networkType = %q, want Calico", data["networkType"])
	}
}

func TestRenderValues(t *testing.T) {
	userFS := fstest.MapFS{
		"with-values/resource.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: config
data:
  env: "{{ .Values.env }}"
  region: "{{ .Values.region }}"
`)},
	}

	coll, err := Load("with-values", userFS, nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := coll.Render(RenderContext{
		Values: map[string]any{"env": "staging", "region": "us-east-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(result.Objects))
	}
	data, _, _ := unstructured.NestedStringMap(result.Objects[0].Object, "data")
	if data["env"] != "staging" {
		t.Errorf("env = %q, want staging", data["env"])
	}
	if data["region"] != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", data["region"])
	}
}

func TestLoadInline(t *testing.T) {
	coll, err := LoadInline("test-inline", []string{
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: from-inline
data:
  key: "{{ .Values.myKey }}"`,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if coll.Name() != "test-inline" {
		t.Errorf("Name() = %q, want test-inline", coll.Name())
	}

	result, err := coll.Render(RenderContext{
		Values: map[string]any{"myKey": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(result.Objects))
	}
	data, _, _ := unstructured.NestedStringMap(result.Objects[0].Object, "data")
	if data["key"] != "hello" {
		t.Errorf("key = %q, want hello", data["key"])
	}
}

func TestLoadInlineEmpty(t *testing.T) {
	_, err := LoadInline("empty", []string{}, nil)
	if err == nil {
		t.Fatal("expected error for empty inline collection")
	}
}

func TestLoadInlineBadTemplate(t *testing.T) {
	_, err := LoadInline("bad", []string{`{{ .Unclosed`}, nil)
	if err == nil {
		t.Fatal("expected error for bad inline template")
	}
}

func TestLoadEmptyCollection(t *testing.T) {
	userFS := fstest.MapFS{
		"empty/.keep": &fstest.MapFile{Data: []byte{}},
	}

	_, err := Load("empty", userFS, nil)
	if err == nil {
		t.Fatal("expected error for empty collection")
	}
}

func TestLoadBadTemplate(t *testing.T) {
	userFS := fstest.MapFS{
		"bad/broken.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Unclosed
`)},
	}

	_, err := Load("bad", userFS, nil)
	if err == nil {
		t.Fatal("expected error for bad template syntax")
	}
}

func TestRenderHashDeterministic(t *testing.T) {
	coll, err := Load("openshift-default", nil, stubCertFuncMap)
	if err != nil {
		t.Fatal(err)
	}
	ctx := RenderContext{
		FeatureSet:   "Default",
		APIServerURL: "https://127.0.0.1:6443",
		Platform:     "None",
		NetworkType:  "OVNKubernetes",
	}
	r1, err := coll.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := coll.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Hash != r2.Hash {
		t.Errorf("same input produced different hashes: %s vs %s", r1.Hash, r2.Hash)
	}
	if r1.Hash == "" {
		t.Error("hash should not be empty")
	}
}

func TestLoadSpecFromBuiltinFS(t *testing.T) {
	fsys, err := BuiltinFS("openshift-default")
	require.NoError(t, err)

	spec := LoadSpec("openshift-default", fsys)
	require.NotNil(t, spec, "LoadSpec should find the builtin collection.yaml")
	assert.Equal(t, "openshift-default", spec.Source)
	assert.Len(t, spec.StatusPatches, 3)
	assert.Equal(t, "Infrastructure", spec.StatusPatches[0].Kind)
	assert.Equal(t, "ClusterVersion", spec.StatusPatches[1].Kind)
	assert.Equal(t, "FeatureGate", spec.StatusPatches[2].Kind)
}

func TestLoadSpecSkipsNonMatchingName(t *testing.T) {
	fsys, err := BuiltinFS("openshift-default")
	require.NoError(t, err)

	spec := LoadSpec("wrong-name", fsys)
	assert.Nil(t, spec)
}

func TestIsKitResourceFiltersCollectionYAML(t *testing.T) {
	fsys, err := BuiltinFS("openshift-default")
	require.NoError(t, err)

	coll, err := loadFromFS("openshift-default", fsys, stubCertFuncMap)
	require.NoError(t, err)

	result, err := coll.Render(RenderContext{Platform: "None", NetworkType: "OVNKubernetes"})
	require.NoError(t, err)
	for _, obj := range result.Objects {
		assert.NotEqual(t, "ResourceCollection", obj.GetKind(), "collection.yaml should be filtered out of templates")
	}
}

func TestRenderHashChangesWithValues(t *testing.T) {
	userFS := fstest.MapFS{
		"val/resource.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
data:
  env: "{{ .Values.env }}"
`)},
	}
	coll, err := Load("val", userFS, nil)
	if err != nil {
		t.Fatal(err)
	}

	r1, err := coll.Render(RenderContext{Values: map[string]any{"env": "dev"}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := coll.Render(RenderContext{Values: map[string]any{"env": "prod"}})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Hash == r2.Hash {
		t.Error("different values should produce different hashes")
	}
}
