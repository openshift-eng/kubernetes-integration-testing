package reconciler_test

import (
	"context"
	"log/slog"
	"testing"
	"testing/fstest"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/reconciler"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// applyHistory is shared between fakes to record the global apply order across clients.
type applyHistory struct {
	ops []string
}

type fakeCRDClient struct {
	applied []string
	crds    map[string]*apiextensionsv1.CustomResourceDefinition
	history *applyHistory
}

func newFakeCRDClient(history *applyHistory) *fakeCRDClient {
	return &fakeCRDClient{crds: make(map[string]*apiextensionsv1.CustomResourceDefinition), history: history}
}

func (f *fakeCRDClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*apiextensionsv1.CustomResourceDefinition, error) {
	crd, ok := f.crds[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, name)
	}
	return crd, nil
}

func (f *fakeCRDClient) Create(_ context.Context, crd *apiextensionsv1.CustomResourceDefinition, _ metav1.CreateOptions) (*apiextensionsv1.CustomResourceDefinition, error) {
	f.applied = append(f.applied, crd.Name)
	f.history.ops = append(f.history.ops, "crd:"+crd.Name)
	crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
	}
	f.crds[crd.Name] = crd
	return crd, nil
}

func (f *fakeCRDClient) Update(_ context.Context, crd *apiextensionsv1.CustomResourceDefinition, _ metav1.UpdateOptions) (*apiextensionsv1.CustomResourceDefinition, error) {
	f.crds[crd.Name] = crd
	return crd, nil
}

func (f *fakeCRDClient) Delete(_ context.Context, name string, _ metav1.DeleteOptions) error {
	delete(f.crds, name)
	return nil
}

type recordingDynClient struct {
	applied      []string
	appliedNames []string
	history      *applyHistory
}

func (r *recordingDynClient) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recordingResource{gvr: resource, rec: r}
}

type recordingResource struct {
	gvr schema.GroupVersionResource
	rec *recordingDynClient
}

func (r *recordingResource) Namespace(string) dynamic.ResourceInterface { return r }

func (r *recordingResource) Get(_ context.Context, _ string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: r.gvr.Group, Resource: r.gvr.Resource}, "")
}

func (r *recordingResource) Create(_ context.Context, obj *unstructured.Unstructured, _ metav1.CreateOptions, _ ...string) (*unstructured.Unstructured, error) {
	r.rec.applied = append(r.rec.applied, obj.GetKind())
	r.rec.appliedNames = append(r.rec.appliedNames, obj.GetName())
	r.rec.history.ops = append(r.rec.history.ops, "dyn:"+obj.GetKind()+"/"+obj.GetName())
	return obj, nil
}

func (r *recordingResource) Update(context.Context, *unstructured.Unstructured, metav1.UpdateOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (r *recordingResource) UpdateStatus(context.Context, *unstructured.Unstructured, metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (r *recordingResource) Delete(context.Context, string, metav1.DeleteOptions, ...string) error {
	return nil
}
func (r *recordingResource) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	return nil
}
func (r *recordingResource) List(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (r *recordingResource) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, nil
}
func (r *recordingResource) Patch(_ context.Context, _ string, _ types.PatchType, body []byte, _ metav1.PatchOptions, _ ...string) (*unstructured.Unstructured, error) {
	var obj unstructured.Unstructured
	if err := obj.UnmarshalJSON(body); err != nil {
		return nil, err
	}
	r.rec.applied = append(r.rec.applied, obj.GetKind())
	r.rec.appliedNames = append(r.rec.appliedNames, obj.GetName())
	r.rec.history.ops = append(r.rec.history.ops, "dyn:"+obj.GetKind()+"/"+obj.GetName())
	return &obj, nil
}
func (r *recordingResource) Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (r *recordingResource) ApplyStatus(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	return nil, nil
}

type fakeDiscovery struct {
	discovery.DiscoveryInterface
}

func (f *fakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return &metav1.APIGroupList{
		Groups: []metav1.APIGroup{
			{
				Name: "example.com",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "example.com/v1", Version: "v1"},
				},
			},
			{
				Name: "admissionregistration.k8s.io",
				Versions: []metav1.GroupVersionForDiscovery{
					{GroupVersion: "admissionregistration.k8s.io/v1", Version: "v1"},
				},
			},
		},
	}, nil
}

func (f *fakeDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	switch groupVersion {
	case "example.com/v1":
		return &metav1.APIResourceList{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{
				{Name: "foos", Kind: "Foo", Namespaced: false},
				{Name: "bars", Kind: "Bar", Namespaced: false},
			},
		}, nil
	case "admissionregistration.k8s.io/v1":
		return &metav1.APIResourceList{
			GroupVersion: "admissionregistration.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "validatingadmissionpolicies", Kind: "ValidatingAdmissionPolicy", Namespaced: false},
				{Name: "validatingadmissionpolicybindings", Kind: "ValidatingAdmissionPolicyBinding", Namespaced: false},
			},
		}, nil
	}
	return &metav1.APIResourceList{}, nil
}

func (f *fakeDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	groups, _ := f.ServerGroups()
	gs := make([]*metav1.APIGroup, len(groups.Groups))
	for i := range groups.Groups {
		gs[i] = &groups.Groups[i]
	}
	var resources []*metav1.APIResourceList
	for _, g := range groups.Groups {
		for _, v := range g.Versions {
			r, _ := f.ServerResourcesForGroupVersion(v.GroupVersion)
			resources = append(resources, r)
		}
	}
	return gs, resources, nil
}

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
