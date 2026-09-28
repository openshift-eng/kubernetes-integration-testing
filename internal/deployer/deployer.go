package deployer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"golang.org/x/sync/errgroup"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	cvoManifestsDir      = "manifests/"
	releaseManifestsDir  = "release-manifests/"
	featureSetAnnotation = "release.openshift.io/feature-set"
	clusterProfile       = "self-managed-high-availability"

	crdEstablishTimeout = 2 * time.Minute
	crdEstablishPoll    = time.Second

	servingCertAnnotation = "service.beta.openshift.io/serving-cert-secret-name"
)

type Opts struct {
	Kubeconfig string
	Image      string
	FeatureSet string
	Includes   []string
	PullSecret string
}

type Deployer struct {
	registry *image.Registry
	log      *slog.Logger
}

func New(registry *image.Registry, log *slog.Logger) *Deployer {
	return &Deployer{registry: registry, log: log}
}

func (d *Deployer) Deploy(ctx context.Context, opts Opts, statusFn func(string)) error {
	includes, err := compilePatterns(opts.Includes)
	if err != nil {
		return fmt.Errorf("compiling include patterns: %w", err)
	}

	statusFn("Extracting manifests from release image")
	allFiles := make(map[string]string)
	for _, dir := range []string{cvoManifestsDir, releaseManifestsDir} {
		files, err := d.registry.ExtractDir(opts.Image, dir)
		if err != nil {
			return fmt.Errorf("extracting %s: %w", dir, err)
		}
		for k, v := range files {
			allFiles[dir+k] = v
		}
	}
	d.log.Info("extracted manifests", "count", len(allFiles))

	statusFn("Parsing and filtering manifests")
	all, err := loadManifests(allFiles)
	if err != nil {
		return fmt.Errorf("loading manifests: %w", err)
	}

	crds, nonCRDs := splitCRDs(all)
	namespaces, filtered := splitNamespaces(filterManifests(nonCRDs, opts.FeatureSet))
	resources := filterByIncludes(filtered, includes)
	d.log.Info("manifest breakdown", "crds", len(crds), "namespaces", len(namespaces), "resources", len(resources))

	cfg, err := clientcmd.BuildConfigFromFlags("", opts.Kubeconfig)
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}
	cfg.QPS = 50
	cfg.Burst = 100

	if err := d.deployCRDs(ctx, cfg, crds, statusFn); err != nil {
		return err
	}

	version := releaseVersion(allFiles, d.log)
	if err := d.ensureBootstrapResources(ctx, cfg, opts, version, statusFn); err != nil {
		return err
	}

	if len(namespaces) > 0 {
		if err := d.deployResources(ctx, cfg, namespaces, statusFn); err != nil {
			return err
		}
	}

	if err := d.ensureServingCerts(ctx, cfg, resources); err != nil {
		return err
	}

	if opts.PullSecret != "" {
		if err := d.ensureGlobalPullSecret(ctx, cfg, opts.PullSecret, statusFn); err != nil {
			return err
		}
	}

	if len(resources) > 0 {
		if err := d.deployResources(ctx, cfg, resources, statusFn); err != nil {
			return err
		}
	}

	return nil
}

func (d *Deployer) deployCRDs(ctx context.Context, cfg *rest.Config, crds []manifest, statusFn func(string)) error {
	extClient, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating apiextensions client: %w", err)
	}

	crdClient := extClient.ApiextensionsV1().CustomResourceDefinitions()
	statusFn(fmt.Sprintf("Applying %d CRDs", len(crds)))

	var mu sync.Mutex
	var applied []string

	applyGroup, applyCtx := errgroup.WithContext(ctx)
	applyGroup.SetLimit(10)

	for _, m := range crds {
		m := m
		applyGroup.Go(func() error {
			var required apiextensionsv1.CustomResourceDefinition
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m.obj.Object, &required); err != nil {
				return fmt.Errorf("converting CRD %s: %w", m.obj.GetName(), err)
			}

			existing, err := crdClient.Get(applyCtx, required.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				d.log.Info("creating CRD", "name", required.Name)
				if _, err := crdClient.Create(applyCtx, &required, metav1.CreateOptions{}); err != nil {
					return fmt.Errorf("creating CRD %s: %w", required.Name, err)
				}
				mu.Lock()
				applied = append(applied, required.Name)
				mu.Unlock()
				return nil
			}
			if err != nil {
				return fmt.Errorf("getting CRD %s: %w", required.Name, err)
			}

			if !equality.Semantic.DeepEqual(existing.Spec, required.Spec) {
				required.ResourceVersion = existing.ResourceVersion
				d.log.Info("updating CRD", "name", required.Name)
				if _, err := crdClient.Update(applyCtx, &required, metav1.UpdateOptions{}); err != nil {
					return fmt.Errorf("updating CRD %s: %w", required.Name, err)
				}
			}
			mu.Lock()
			applied = append(applied, required.Name)
			mu.Unlock()
			return nil
		})
	}

	if err := applyGroup.Wait(); err != nil {
		return err
	}

	statusFn("Waiting for CRDs to be established")

	waitGroup, waitCtx := errgroup.WithContext(ctx)
	for _, name := range applied {
		name := name
		waitGroup.Go(func() error {
			d.log.Info("waiting for CRD", "name", name)
			if err := wait.PollUntilContextTimeout(waitCtx, crdEstablishPoll, crdEstablishTimeout, true, func(ctx context.Context) (bool, error) {
				crd, err := crdClient.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				for _, c := range crd.Status.Conditions {
					if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
						return true, nil
					}
				}
				return false, nil
			}); err != nil {
				return fmt.Errorf("waiting for CRD %s: %w", name, err)
			}
			return nil
		})
	}

	return waitGroup.Wait()
}

func (d *Deployer) deployResources(ctx context.Context, cfg *rest.Config, resources []manifest, statusFn func(string)) error {
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating discovery client: %w", err)
	}

	groupResources, err := restmapper.GetAPIGroupResources(disc)
	if err != nil {
		return fmt.Errorf("discovering API resources: %w", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	statusFn(fmt.Sprintf("Applying %d resources", len(resources)))

	var failed int
	for _, m := range resources {
		gvk := m.obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			d.log.Warn("skipping unknown resource type", "gvk", gvk.String(), "file", m.filename, "error", err)
			continue
		}

		var client dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := m.obj.GetNamespace()
			if ns == "" {
				ns = "default"
			}
			client = dynClient.Resource(mapping.Resource).Namespace(ns)
		} else {
			client = dynClient.Resource(mapping.Resource)
		}

		if err := applyOne(ctx, client, m.obj); err != nil {
			d.log.Warn("failed to apply resource", "gvk", gvk.String(), "name", m.obj.GetName(), "file", m.filename, "error", err)
			failed++
		} else {
			d.log.Info("applied resource", "gvk", gvk.String(), "name", m.obj.GetName())
		}
	}

	if failed > 0 {
		d.log.Warn("some resources failed to apply", "failed", failed, "total", len(resources))
	}
	return nil
}

func (d *Deployer) ensureBootstrapResources(ctx context.Context, cfg *rest.Config, opts Opts, version string, statusFn func(string)) error {
	statusFn("Creating bootstrap resources")

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	configRes := func(resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: resource}
	}

	// TODO(pabrodri): these bootstrap resources may become user-provided in the future.
	bootstrapResources := []struct {
		gvr schema.GroupVersionResource
		obj *unstructured.Unstructured
	}{
		{configRes("featuregates"), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "FeatureGate",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec": map[string]interface{}{
				"featureSet": opts.FeatureSet,
			},
		}}},
		{configRes("clusterversions"), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "ClusterVersion",
			"metadata":   map[string]interface{}{"name": "version"},
			"spec": map[string]interface{}{
				"clusterID": "",
				"channel":   "",
			},
		}}},
		{configRes("images"), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "Image",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		}}},
		{configRes("infrastructures"), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "Infrastructure",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
			"status": map[string]interface{}{
				"platform": "None",
				"platformStatus": map[string]interface{}{
					"type": "None",
				},
				"apiServerInternalURL": apiServerURL(opts.Kubeconfig),
			},
		}}},
		{configRes("dnses"), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "DNS",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		}}},
		{configRes("proxies"), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "Proxy",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		}}},
		{configRes("apiservers"), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "APIServer",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		}}},
		{configRes("networks"), &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec": map[string]interface{}{
				"serviceNetwork": []interface{}{"10.96.0.0/16"},
				"networkType":    "OVNKubernetes",
			},
		}}},
	}

	for _, r := range bootstrapResources {
		if err := applyOne(ctx, dynClient.Resource(r.gvr), r.obj); err != nil {
			return fmt.Errorf("creating %s %s: %w", r.obj.GetKind(), r.obj.GetName(), err)
		}
		d.log.Info("ensured bootstrap resource", "kind", r.obj.GetKind(), "name", r.obj.GetName())
	}

	// Infrastructure status must be set via the status subresource.
	infraRes := configRes("infrastructures")
	infra, err := dynClient.Resource(infraRes).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting Infrastructure for status update: %w", err)
	}
	infra.Object["status"] = map[string]interface{}{
		"platform": "None",
		"platformStatus": map[string]interface{}{
			"type": "None",
		},
		"apiServerInternalURL": apiServerURL(opts.Kubeconfig),
	}
	if _, err := dynClient.Resource(infraRes).UpdateStatus(ctx, infra, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating Infrastructure status: %w", err)
	}

	if version != "" {
		fgRes := configRes("featuregates")
		fg, err := dynClient.Resource(fgRes).Get(ctx, "cluster", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting FeatureGate for status update: %w", err)
		}
		unstructured.SetNestedSlice(fg.Object, []interface{}{
			map[string]interface{}{
				"version":  version,
				"enabled":  []interface{}{},
				"disabled": []interface{}{},
			},
		}, "status", "featureGates")
		if _, err := dynClient.Resource(fgRes).UpdateStatus(ctx, fg, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating FeatureGate status: %w", err)
		}
		d.log.Info("set FeatureGate status", "version", version)
	}

	return nil
}

func (d *Deployer) ensureServingCerts(ctx context.Context, cfg *rest.Config, resources []manifest) error {
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	clusterCA := string(cfg.CAData)
	if clusterCA == "" && cfg.CAFile != "" {
		raw, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return fmt.Errorf("reading CA file: %w", err)
		}
		clusterCA = string(raw)
	}

	servingCA, err := generateServingCA()
	if err != nil {
		return fmt.Errorf("generating serving CA: %w", err)
	}

	caBundle := clusterCA + servingCA.certPEM

	cmRes := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	caConfigMaps := []struct {
		namespace string
		name      string
		key       string
	}{
		{"kube-system", "root-ca", "ca.crt"},
		{"openshift-config", "initial-kube-apiserver-server-ca", "ca-bundle.crt"},
	}

	for _, cm := range caConfigMaps {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      cm.name,
				"namespace": cm.namespace,
			},
			"data": map[string]interface{}{
				cm.key: caBundle,
			},
		}}
		if err := applyOne(ctx, dynClient.Resource(cmRes).Namespace(cm.namespace), obj); err != nil {
			return fmt.Errorf("creating configmap %s/%s: %w", cm.namespace, cm.name, err)
		}
		d.log.Info("ensured CA configmap", "namespace", cm.namespace, "name", cm.name)
	}

	secrets := collectServingCertSecrets(resources)
	if len(secrets) == 0 {
		return nil
	}

	secretRes := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	for _, s := range secrets {
		certPEM, keyPEM, err := generateServingCert(servingCA, s.name, s.namespace)
		if err != nil {
			return fmt.Errorf("generating serving cert for %s/%s: %w", s.namespace, s.name, err)
		}

		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      s.name,
				"namespace": s.namespace,
			},
			"type": "kubernetes.io/tls",
			"data": map[string]interface{}{
				"tls.crt": certPEM,
				"tls.key": keyPEM,
			},
		}}
		if err := applyOne(ctx, dynClient.Resource(secretRes).Namespace(s.namespace), obj); err != nil {
			return fmt.Errorf("creating serving cert secret %s/%s: %w", s.namespace, s.name, err)
		}
		d.log.Info("ensured serving cert secret", "namespace", s.namespace, "name", s.name)
	}

	return nil
}

type servingCertCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM string
}

func generateServingCA() (*servingCertCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kit-serving-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return &servingCertCA{cert: cert, key: key, certPEM: string(certPEM)}, nil
}

func generateServingCert(ca *servingCertCA, name, namespace string) (certB64, keyB64 string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}

	dnsNames := []string{
		name,
		fmt.Sprintf("%s.%s", name, namespace),
		fmt.Sprintf("%s.%s.svc", name, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace),
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: fmt.Sprintf("%s.%s.svc", name, namespace)},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return "", "", err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return base64Encode(certPEM), base64Encode(keyPEM), nil
}

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

type servingCertSecret struct {
	name      string
	namespace string
}

func collectServingCertSecrets(resources []manifest) []servingCertSecret {
	var secrets []servingCertSecret
	seen := make(map[string]bool)
	for _, m := range resources {
		if m.obj.GetKind() != "Service" {
			continue
		}
		annotations := m.obj.GetAnnotations()
		secretName, ok := annotations[servingCertAnnotation]
		if !ok || secretName == "" {
			continue
		}
		ns := m.obj.GetNamespace()
		key := ns + "/" + secretName
		if seen[key] {
			continue
		}
		seen[key] = true
		secrets = append(secrets, servingCertSecret{name: secretName, namespace: ns})
	}
	return secrets
}

func releaseVersion(files map[string]string, log *slog.Logger) string {
	path, ok := files[releaseManifestsDir+"image-references"]
	if !ok {
		log.Warn("image-references not found, cannot determine release version")
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warn("reading image-references", "error", err)
		return ""
	}
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		log.Warn("parsing image-references", "error", err)
		return ""
	}
	log.Info("detected release version", "version", meta.Metadata.Name)
	return meta.Metadata.Name
}

func apiServerURL(kubeconfigPath string) string {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return ""
	}
	return cfg.Host
}

func applyOne(ctx context.Context, client dynamic.ResourceInterface, obj *unstructured.Unstructured) error {
	existing, err := client.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.Create(ctx, obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	_, err = client.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

type manifest struct {
	filename string
	obj      *unstructured.Unstructured
}

func loadManifests(files map[string]string) ([]manifest, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []manifest
	for _, name := range names {
		if !isManifestFile(name) {
			continue
		}

		data, err := os.ReadFile(files[name])
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}

		objs, err := parseMultiDoc(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}

		for _, obj := range objs {
			out = append(out, manifest{filename: name, obj: obj})
		}
	}
	return out, nil
}

func filterManifests(manifests []manifest, featureSet string) []manifest {
	var out []manifest
	for _, m := range manifests {
		if includeManifest(m.obj.GetAnnotations(), featureSet) {
			out = append(out, m)
		}
	}
	return out
}

func parseMultiDoc(data []byte) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if obj.GetAPIVersion() == "" || obj.GetKind() == "" {
			continue
		}
		objects = append(objects, obj)
	}
	return objects, nil
}

// includeManifest replicates the CVO's manifest.Include filtering:
//   - No annotations → exclude
//   - Deletion stubs (release.openshift.io/delete=true) → exclude
//   - Profile: include.release.openshift.io/self-managed-high-availability must be "true"
//   - FeatureSet: if release.openshift.io/feature-set is present, required value must be in the list
func includeManifest(annotations map[string]string, requiredFeatureSet string) bool {
	if annotations == nil {
		return false
	}

	if annotations["release.openshift.io/delete"] == "true" {
		return false
	}

	profileKey := fmt.Sprintf("include.release.openshift.io/%s", clusterProfile)
	if v, ok := annotations[profileKey]; !ok || v != "true" {
		return false
	}

	fsValue, hasFS := annotations[featureSetAnnotation]
	if !hasFS {
		return true
	}

	required := requiredFeatureSet
	if required == "" {
		required = "Default"
	}
	for _, v := range strings.Split(fsValue, ",") {
		if strings.TrimSpace(v) == required {
			return true
		}
	}
	return false
}

func splitCRDs(manifests []manifest) (crds, rest []manifest) {
	seen := make(map[string]int)
	for _, m := range manifests {
		gvk := m.obj.GroupVersionKind()
		if strings.HasSuffix(gvk.Group, "apiextensions.k8s.io") && gvk.Kind == "CustomResourceDefinition" {
			name := m.obj.GetName()
			if idx, ok := seen[name]; ok {
				crds[idx] = m
			} else {
				seen[name] = len(crds)
				crds = append(crds, m)
			}
		} else {
			rest = append(rest, m)
		}
	}
	return
}

func filterByIncludes(resources []manifest, patterns []*regexp.Regexp) []manifest {
	if len(patterns) == 0 {
		return nil
	}
	var out []manifest
	for _, m := range resources {
		for _, p := range patterns {
			if p.MatchString(m.filename) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
		compiled[i] = re
	}
	return compiled, nil
}

func (d *Deployer) ensureGlobalPullSecret(ctx context.Context, cfg *rest.Config, pullSecretPath string, statusFn func(string)) error {
	data, err := os.ReadFile(pullSecretPath)
	if err != nil {
		return fmt.Errorf("reading pull secret: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	statusFn("Creating global pull secret in openshift-config")

	secretsRes := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      "pull-secret",
			"namespace": "openshift-config",
		},
		"type": "kubernetes.io/dockerconfigjson",
		"stringData": map[string]interface{}{
			".dockerconfigjson": string(data),
		},
	}}

	if err := applyOne(ctx, dynClient.Resource(secretsRes).Namespace("openshift-config"), secret); err != nil {
		return fmt.Errorf("creating global pull secret: %w", err)
	}
	d.log.Info("ensured global pull secret", "namespace", "openshift-config")
	return nil
}

func splitNamespaces(resources []manifest) (namespaces, rest []manifest) {
	for _, m := range resources {
		if m.obj.GetKind() == "Namespace" {
			namespaces = append(namespaces, m)
		} else {
			rest = append(rest, m)
		}
	}
	return
}

func collectNamespaces(resources []manifest) []string {
	seen := make(map[string]bool)
	for _, m := range resources {
		if m.obj.GetKind() == "Namespace" {
			if n := m.obj.GetName(); n != "" {
				seen[n] = true
			}
			continue
		}
		if ns := m.obj.GetNamespace(); ns != "" {
			seen[ns] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

func isManifestFile(name string) bool {
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}
