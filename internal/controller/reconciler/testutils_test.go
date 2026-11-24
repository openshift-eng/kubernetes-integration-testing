// Shared test fakes for the reconciler tests.
//
// Every _test.go file in a package compiles together, so helpers defined
// here are visible to all of them. Cross-file test infrastructure lives
// in this file on purpose: finding a fake used by another test file
// should never require knowing which test happened to define it first.

package reconciler_test

import (
	"context"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"

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

// recordingClients returns the standard fake cluster clients for
// reconciler tests: a recording dynamic client and a discovery client
// serving the example.com and admissionregistration.k8s.io groups the
// collection templates render against.
func recordingClients() (*kube.Clients, *recordingDynClient) {
	dyn := &recordingDynClient{history: &applyHistory{}}
	return &kube.Clients{Dynamic: dyn, Discovery: &fakeDiscovery{}}, dyn
}

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

// recordingDynClient is a dynamic.Interface that records every created
// or patched object. applyErr, when set, makes every create and patch
// fail.
type recordingDynClient struct {
	applied      []string
	appliedNames []string
	history      *applyHistory
	applyErr     error
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
	if r.rec.applyErr != nil {
		return nil, r.rec.applyErr
	}
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
	if r.rec.applyErr != nil {
		return nil, r.rec.applyErr
	}
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

// fakeDiscovery serves the API groups the test templates render against:
// example.com (Foo, Bar) and admissionregistration.k8s.io.
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
