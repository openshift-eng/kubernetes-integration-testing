package reconciler_test

import (
	"context"
	"log/slog"
	"testing"
	"testing/fstest"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"
)

func TestInstallOrdersByTierThenFilename(t *testing.T) {
	crdYAML := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: foos.example.com
spec:
  group: example.com
  names:
    kind: Foo
    plural: foos
  scope: Cluster
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`
	crYAML1 := `apiVersion: example.com/v1
kind: Foo
metadata:
  name: second
`
	crYAML2 := `apiVersion: example.com/v1
kind: Foo
metadata:
  name: first
`
	vapYAML := `apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: test-policy
`

	// VAP sorts first alphabetically (00) but belongs to tier 2, CRD (02) is tier 0, CRs (03, 04) are tier 1.
	// The installer must reorder by tier: CRD → CRs (filename order) → VAP.
	fsys := fstest.MapFS{
		"00-vap.yaml":     {Data: []byte(vapYAML)},
		"02-crd.yaml":     {Data: []byte(crdYAML)},
		"03-cr.yaml":      {Data: []byte(crYAML1)},
		"04-another.yaml": {Data: []byte(crYAML2)},
	}

	history := &applyHistory{}
	crdFake := newFakeCRDClient(history)
	dynFake := &recordingDynClient{history: history}

	installer := reconciler.NewManifestInstaller(crdFake, dynFake, &fakeDiscovery{}, slog.Default())

	if err := installer.Install(context.Background(), fsys); err != nil {
		t.Fatal(err)
	}

	wantOps := []string{
		"crd:foos.example.com",
		"dyn:Foo/second",
		"dyn:Foo/first",
		"dyn:ValidatingAdmissionPolicy/test-policy",
	}
	if len(history.ops) != len(wantOps) {
		t.Fatalf("expected %d ops, got %v", len(wantOps), history.ops)
	}
	for i, want := range wantOps {
		if history.ops[i] != want {
			t.Errorf("op[%d]: got %q, want %q", i, history.ops[i], want)
		}
	}
}

func TestInstallCRDsBeforeVAPs(t *testing.T) {
	crdYAML := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: foos.example.com
spec:
  group: example.com
  names:
    kind: Foo
    plural: foos
  scope: Cluster
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`
	vapYAML := `apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: test-policy
`

	fsys := fstest.MapFS{
		"01-vap.yaml": {Data: []byte(vapYAML)},
		"02-crd.yaml": {Data: []byte(crdYAML)},
	}

	history := &applyHistory{}
	crdFake := newFakeCRDClient(history)
	dynFake := &recordingDynClient{history: history}

	installer := reconciler.NewManifestInstaller(crdFake, dynFake, &fakeDiscovery{}, slog.Default())

	if err := installer.Install(context.Background(), fsys); err != nil {
		t.Fatal(err)
	}

	wantOps := []string{
		"crd:foos.example.com",
		"dyn:ValidatingAdmissionPolicy/test-policy",
	}
	if len(history.ops) != len(wantOps) {
		t.Fatalf("expected %d ops, got %v", len(wantOps), history.ops)
	}
	for i, want := range wantOps {
		if history.ops[i] != want {
			t.Errorf("op[%d]: got %q, want %q", i, history.ops[i], want)
		}
	}
}

func TestInstallEmptyFS(t *testing.T) {
	fsys := fstest.MapFS{}

	history := &applyHistory{}
	installer := reconciler.NewManifestInstaller(newFakeCRDClient(history), &recordingDynClient{history: history}, &fakeDiscovery{}, slog.Default())

	if err := installer.Install(context.Background(), fsys); err != nil {
		t.Fatalf("empty fs should not error: %v", err)
	}
}
