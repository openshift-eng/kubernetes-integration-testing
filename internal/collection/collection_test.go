package collection

import (
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestLoadBuiltin(t *testing.T) {
	coll, err := Load("openshift-default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if coll.Name() != "openshift-default" {
		t.Errorf("Name() = %q, want openshift-default", coll.Name())
	}
}

func TestLoadUnknown(t *testing.T) {
	_, err := Load("nonexistent", nil)
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

	coll, err := Load("my-profile", userFS)
	if err != nil {
		t.Fatal(err)
	}
	if coll.Name() != "my-profile" {
		t.Errorf("Name() = %q, want my-profile", coll.Name())
	}

	objects, err := coll.Render(RenderContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(objects))
	}
	if objects[0].GetKind() != "ConfigMap" || objects[0].GetName() != "test" {
		t.Errorf("got %s/%s, want ConfigMap/test", objects[0].GetKind(), objects[0].GetName())
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

	coll, err := Load("openshift-default", userFS)
	if err != nil {
		t.Fatal(err)
	}

	objects, err := coll.Render(RenderContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Errorf("got %d objects, want 1 (user override should replace builtin)", len(objects))
	}
}

func TestRenderBuiltinObjects(t *testing.T) {
	coll, err := Load("openshift-default", nil)
	if err != nil {
		t.Fatal(err)
	}

	objects, err := coll.Render(RenderContext{
		FeatureSet:      "TechPreviewNoUpgrade",
		APIServerURL:    "https://127.0.0.1:6443",
		Platform:        "None",
		ServiceNetworks: []string{"10.96.0.0/16"},
		NetworkType:     "OVNKubernetes",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(objects) != 8 {
		t.Fatalf("got %d objects, want 8", len(objects))
	}

	expected := []string{"APIServer", "ClusterVersion", "DNS", "FeatureGate", "Image", "Infrastructure", "Network", "Proxy"}
	found := make(map[string]bool)
	for _, obj := range objects {
		found[obj.GetKind()] = true
	}
	for _, kind := range expected {
		if !found[kind] {
			t.Errorf("missing expected kind %q", kind)
		}
	}
}

func TestRenderTemplateValues(t *testing.T) {
	coll, err := Load("openshift-default", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := RenderContext{
		FeatureSet:      "TechPreviewNoUpgrade",
		APIServerURL:    "https://10.0.0.1:6443",
		Platform:        "AWS",
		ServiceNetworks: []string{"10.128.0.0/14"},
		NetworkType:     "OpenShiftSDN",
	}

	objects, err := coll.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, obj := range objects {
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

	coll, err := Load("templated", userFS)
	if err != nil {
		t.Fatal(err)
	}

	objects, err := coll.Render(RenderContext{Platform: "GCP", NetworkType: "Calico"})
	if err != nil {
		t.Fatal(err)
	}

	if len(objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(objects))
	}
	data, _, _ := unstructured.NestedStringMap(objects[0].Object, "data")
	if data["platform"] != "GCP" {
		t.Errorf("platform = %q, want GCP", data["platform"])
	}
	if data["networkType"] != "Calico" {
		t.Errorf("networkType = %q, want Calico", data["networkType"])
	}
}

func TestLoadEmptyCollection(t *testing.T) {
	userFS := fstest.MapFS{
		"empty/.keep": &fstest.MapFile{Data: []byte{}},
	}

	_, err := Load("empty", userFS)
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

	_, err := Load("bad", userFS)
	if err == nil {
		t.Fatal("expected error for bad template syntax")
	}
}
