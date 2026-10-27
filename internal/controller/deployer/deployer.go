package deployer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/image"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"


	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

type Opts struct {
	KubeConfig *kube.KubeConfig
	Image      string
	FeatureSet string
	Includes   []string
}

type Deployer struct {
	registry *image.Registry
	log      *slog.Logger
}

func New(registry *image.Registry, log *slog.Logger) *Deployer {
	return &Deployer{registry: registry, log: log}
}

func (d *Deployer) Deploy(ctx context.Context, opts Opts, statusFn func(string)) error {
	statusFn("Extracting manifests from release image")
	rel := release.NewReleaseManifests(d.registry, d.log)
	if err := rel.Load(opts.Image); err != nil {
		return fmt.Errorf("loading release: %w", err)
	}

	statusFn("Parsing and filtering manifests")
	all, err := rel.All()
	if err != nil {
		return fmt.Errorf("loading manifests: %w", err)
	}

	_, nonCRDs := release.SplitCRDs(all)
	namespaces, resources, err := release.FilterResources(nonCRDs, opts.FeatureSet, opts.Includes)
	if err != nil {
		return fmt.Errorf("filtering manifests: %w", err)
	}
	d.log.Info("manifest breakdown", "namespaces", len(namespaces), "resources", len(resources))

	cfg, err := opts.KubeConfig.RestConfig()
	if err != nil {
		return fmt.Errorf("building kubeconfig: %w", err)
	}
	cfg.QPS = 50
	cfg.Burst = 100

	version := rel.Version()
	if err := d.patchBootstrapStatus(ctx, cfg, opts, version, all, statusFn); err != nil {
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

	if len(resources) > 0 {
		if err := d.deployResources(ctx, cfg, resources, statusFn); err != nil {
			return err
		}
	}

	return nil
}

func (d *Deployer) deployResources(ctx context.Context, cfg *rest.Config, resources []release.Manifest, statusFn func(string)) error {
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
		gvk := m.Obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			d.log.Warn("skipping unknown resource type", "gvk", gvk.String(), "file", m.Filename, "error", err)
			continue
		}

		var client dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := m.Obj.GetNamespace()
			if ns == "" {
				ns = "default"
			}
			client = dynClient.Resource(mapping.Resource).Namespace(ns)
		} else {
			client = dynClient.Resource(mapping.Resource)
		}

		if err := applyOne(ctx, client, m.Obj); err != nil {
			d.log.Warn("failed to apply resource", "gvk", gvk.String(), "name", m.Obj.GetName(), "file", m.Filename, "error", err)
			failed++
		} else {
			d.log.Info("applied resource", "gvk", gvk.String(), "name", m.Obj.GetName())
		}
	}

	if failed > 0 {
		d.log.Warn("some resources failed to apply", "failed", failed, "total", len(resources))
	}
	return nil
}

func (d *Deployer) patchBootstrapStatus(ctx context.Context, cfg *rest.Config, opts Opts, version string, allManifests []release.Manifest, statusFn func(string)) error {
	statusFn("Patching bootstrap resource status")

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	configRes := func(resource string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: resource}
	}

	infraRes := configRes("infrastructures")
	infra, err := dynClient.Resource(infraRes).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting Infrastructure for status update: %w", err)
	}
	apiURL := apiServerURL(opts.KubeConfig)
	infra.Object["status"] = map[string]interface{}{
		"platform": "None",
		"platformStatus": map[string]interface{}{
			"type": "None",
		},
		"apiServerInternalURI": apiURL,
	}
	if _, err := dynClient.Resource(infraRes).UpdateStatus(ctx, infra, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating Infrastructure status: %w", err)
	}

	cvRes := configRes("clusterversions")
	cv, err := dynClient.Resource(cvRes).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting ClusterVersion for status update: %w", err)
	}
	cv.Object["status"] = map[string]interface{}{
		"desired": map[string]interface{}{
			"image":   opts.Image,
			"version": version,
		},
		"observedGeneration": 0,
		"versionHash":        "",
		"availableUpdates":   []interface{}{},
	}
	if _, err := dynClient.Resource(cvRes).UpdateStatus(ctx, cv, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating ClusterVersion status: %w", err)
	}

	if fgStatus := release.FindFeatureGateStatus(allManifests, opts.FeatureSet); fgStatus != nil {
		if version != "" {
			if gates, ok := fgStatus["featureGates"].([]interface{}); ok {
				for _, g := range gates {
					if entry, ok := g.(map[string]interface{}); ok {
						entry["version"] = version
					}
				}
			}
		}
		fgRes := configRes("featuregates")
		fg, err := dynClient.Resource(fgRes).Get(ctx, "cluster", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting FeatureGate for status update: %w", err)
		}
		fg.Object["status"] = fgStatus
		if _, err := dynClient.Resource(fgRes).UpdateStatus(ctx, fg, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating FeatureGate status: %w", err)
		}
		d.log.Info("set FeatureGate status from release manifest", "version", version)
	}

	return nil
}

func (d *Deployer) ensureServingCerts(ctx context.Context, cfg *rest.Config, resources []release.Manifest) error {
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

	secrets := release.CollectServingCertSecrets(resources)
	if len(secrets) == 0 {
		return nil
	}

	secretRes := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	for _, s := range secrets {
		certPEM, keyPEM, err := generateServingCert(servingCA, s.Name, s.Namespace)
		if err != nil {
			return fmt.Errorf("generating serving cert for %s/%s: %w", s.Namespace, s.Name, err)
		}

		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      s.Name,
				"namespace": s.Namespace,
			},
			"type": "kubernetes.io/tls",
			"data": map[string]interface{}{
				"tls.crt": certPEM,
				"tls.key": keyPEM,
			},
		}}
		if err := applyOne(ctx, dynClient.Resource(secretRes).Namespace(s.Namespace), obj); err != nil {
			return fmt.Errorf("creating serving cert secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		d.log.Info("ensured serving cert secret", "namespace", s.Namespace, "name", s.Name)
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

func apiServerURL(kc *kube.KubeConfig) string {
	cfg, err := kc.RestConfig()
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
