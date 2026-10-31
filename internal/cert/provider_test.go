package cert

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fake "k8s.io/client-go/kubernetes/fake"
)

func newTestProvider() *Provider {
	return NewProvider(fake.NewSimpleClientset(), "test-ns")
}

func TestProviderCACertAutoGenerates(t *testing.T) {
	p := newTestProvider()

	pem1, err := p.CACert("test-ca")
	require.NoError(t, err)
	assert.Contains(t, pem1, "BEGIN CERTIFICATE")

	pem2, err := p.CACert("test-ca")
	require.NoError(t, err)
	assert.Equal(t, pem1, pem2, "same CA name should return same cert")
}

func TestProviderSetCA(t *testing.T) {
	p := newTestProvider()

	ca, err := GenerateCA("preset", DefaultValidity)
	require.NoError(t, err)
	p.SetCA("cluster-ca", ca)

	pem, err := p.CACert("cluster-ca")
	require.NoError(t, err)
	assert.Equal(t, ca.CertPEM, pem)
}

func TestProviderSignedCertAndKey(t *testing.T) {
	p := newTestProvider()

	certPEM, err := p.SignedCert("my-ca", "my-service.ns.svc")
	require.NoError(t, err)
	assert.Contains(t, certPEM, "BEGIN CERTIFICATE")

	keyPEM, err := p.SignedKey("my-ca", "my-service.ns.svc")
	require.NoError(t, err)
	assert.Contains(t, keyPEM, "BEGIN EC PRIVATE KEY")

	certPEM2, err := p.SignedCert("my-ca", "my-service.ns.svc")
	require.NoError(t, err)
	assert.Equal(t, certPEM, certPEM2, "same ca+cn should return cached leaf")
}

func TestProviderSANChangeRegenerates(t *testing.T) {
	p := newTestProvider()

	// Generate cert with normal SANs.
	certPEM1, err := p.SignedCert("my-ca", "svc.ns.svc", "svc.ns.svc", "svc.ns.svc.cluster.local")
	require.NoError(t, err)

	// Request same CN but different SANs (headless: adds wildcard).
	certPEM2, err := p.SignedCert("my-ca", "svc.ns.svc", "svc.ns.svc", "svc.ns.svc.cluster.local", "*.svc.ns.svc", "*.svc.ns.svc.cluster.local")
	require.NoError(t, err)
	assert.NotEqual(t, certPEM1, certPEM2, "different SANs should produce a new cert")

	// Verify the new cert has the wildcard SANs.
	block, _ := pem.Decode([]byte(certPEM2))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Contains(t, cert.DNSNames, "*.svc.ns.svc")

	// Verify the persisted Secret was updated.
	secretName := leafSecretName("my-ca", "svc.ns.svc")
	secret, err := p.clientset.CoreV1().Secrets("test-ns").Get(context.TODO(), secretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, certPEM2, string(secret.Data[corev1.TLSCertKey]))
}

func TestProviderExpiredCertRegenerates(t *testing.T) {
	p := newTestProvider()

	// Generate a valid cert first.
	certPEM1, err := p.SignedCert("my-ca", "svc.ns.svc")
	require.NoError(t, err)

	// Manually create an expired cert in the Secret.
	ca, err := p.GetCA("my-ca")
	require.NoError(t, err)
	expired, err := GenerateLeafCert(ca, "svc.ns.svc", []string{"svc.ns.svc"}, -1)
	require.NoError(t, err)

	secretName := leafSecretName("my-ca", "svc.ns.svc")
	secret, err := p.clientset.CoreV1().Secrets("test-ns").Get(context.TODO(), secretName, metav1.GetOptions{})
	require.NoError(t, err)
	secret.Data[corev1.TLSCertKey] = []byte(expired.CertPEM)
	secret.Data[corev1.TLSPrivateKeyKey] = []byte(expired.KeyPEM)
	_, err = p.clientset.CoreV1().Secrets("test-ns").Update(context.TODO(), secret, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Clear cache to force reload from Secret.
	delete(p.leafCerts, "my-ca/svc.ns.svc")

	certPEM2, err := p.SignedCert("my-ca", "svc.ns.svc")
	require.NoError(t, err)
	assert.NotEqual(t, certPEM1, certPEM2, "expired cert should be regenerated")
	assert.Contains(t, certPEM2, "BEGIN CERTIFICATE")
}

func TestProviderMalformedCertRegenerates(t *testing.T) {
	p := newTestProvider()

	// Generate a valid cert first.
	_, err := p.SignedCert("my-ca", "svc.ns.svc")
	require.NoError(t, err)

	// Corrupt the cert in the Secret.
	secretName := leafSecretName("my-ca", "svc.ns.svc")
	secret, err := p.clientset.CoreV1().Secrets("test-ns").Get(context.TODO(), secretName, metav1.GetOptions{})
	require.NoError(t, err)
	secret.Data[corev1.TLSCertKey] = []byte("not-a-pem")
	_, err = p.clientset.CoreV1().Secrets("test-ns").Update(context.TODO(), secret, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Clear cache to force reload from Secret.
	delete(p.leafCerts, "my-ca/svc.ns.svc")

	certPEM, err := p.SignedCert("my-ca", "svc.ns.svc")
	require.NoError(t, err)
	assert.Contains(t, certPEM, "BEGIN CERTIFICATE", "malformed cert should be auto-repaired")

	// Verify the Secret was repaired.
	secret, err = p.clientset.CoreV1().Secrets("test-ns").Get(context.TODO(), secretName, metav1.GetOptions{})
	require.NoError(t, err)
	block, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
	assert.NotNil(t, block, "persisted Secret should have valid PEM after repair")
}

func TestProviderFuncMapInTemplate(t *testing.T) {
	p := newTestProvider()

	ca, err := GenerateCA("cluster-ca", DefaultValidity)
	require.NoError(t, err)
	p.SetCA("cluster-ca", ca)

	src := `ca: {{ caCert "cluster-ca" }}`
	funcMap := template.FuncMap{"caCert": p.CACert}
	tmpl, err := template.New("test").Funcs(funcMap).Parse(src)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, tmpl.Execute(&buf, nil))
	assert.Contains(t, buf.String(), "BEGIN CERTIFICATE")
}
