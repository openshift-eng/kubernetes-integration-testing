package kube

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func generatePKI(t *testing.T, dir string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	writePEM(t, filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER)

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-admin"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	require.NoError(t, err)
	writePEM(t, filepath.Join(dir, "admin.crt"), "CERTIFICATE", clientDER)

	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	require.NoError(t, err)
	writePEM(t, filepath.Join(dir, "admin.key"), "EC PRIVATE KEY", keyDER)
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	require.NoError(t, pem.Encode(f, &pem.Block{Type: typ, Bytes: der}))
}

func writeKubeconfig(t *testing.T) (kubeconfigPath string, pkiDir string) {
	t.Helper()
	dir := t.TempDir()
	pki := filepath.Join(dir, "pki")
	generatePKI(t, pki)

	kc := filepath.Join(dir, "kubeconfig")
	content := `apiVersion: v1
clusters:
- cluster:
    certificate-authority: ` + filepath.Join(pki, "ca.crt") + `
    server: https://127.0.0.1:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
kind: Config
users:
- name: test
  user:
    client-certificate: ` + filepath.Join(pki, "admin.crt") + `
    client-key: ` + filepath.Join(pki, "admin.key") + `
`
	require.NoError(t, os.WriteFile(kc, []byte(content), 0o600))
	return kc, pki
}

func TestRestConfigInlinesTLSData(t *testing.T) {
	kcPath, pki := writeKubeconfig(t)

	kc, err := New(kcPath, "")
	require.NoError(t, err)

	cfg, err := kc.RestConfig()
	require.NoError(t, err)

	caOnDisk, _ := os.ReadFile(filepath.Join(pki, "ca.crt"))
	certOnDisk, _ := os.ReadFile(filepath.Join(pki, "admin.crt"))
	keyOnDisk, _ := os.ReadFile(filepath.Join(pki, "admin.key"))

	assert.Equal(t, caOnDisk, cfg.CAData, "CAData should match CA file on disk")
	assert.Equal(t, certOnDisk, cfg.CertData, "CertData should match cert file on disk")
	assert.Equal(t, keyOnDisk, cfg.KeyData, "KeyData should match key file on disk")
}

func TestRestConfigClearsFilePaths(t *testing.T) {
	kcPath, _ := writeKubeconfig(t)

	kc, err := New(kcPath, "")
	require.NoError(t, err)

	cfg, err := kc.RestConfig()
	require.NoError(t, err)

	assert.Empty(t, cfg.CAFile)
	assert.Empty(t, cfg.CertFile)
	assert.Empty(t, cfg.KeyFile)
}

func TestRestConfigPreservesExistingData(t *testing.T) {
	kcPath, pki := writeKubeconfig(t)

	caOnDisk, err := os.ReadFile(filepath.Join(pki, "ca.crt"))
	require.NoError(t, err)

	content := `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ` + base64.StdEncoding.EncodeToString(caOnDisk) + `
    server: https://127.0.0.1:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
kind: Config
users:
- name: test
  user:
    client-certificate: ` + filepath.Join(pki, "admin.crt") + `
    client-key: ` + filepath.Join(pki, "admin.key") + `
`
	require.NoError(t, os.WriteFile(kcPath, []byte(content), 0o600))

	kc, err := New(kcPath, "")
	require.NoError(t, err)

	cfg, err := kc.RestConfig()
	require.NoError(t, err)

	assert.Equal(t, caOnDisk, cfg.CAData, "inline CAData should be preserved")
}

func TestRestConfigMissingFile(t *testing.T) {
	kcPath := filepath.Join(t.TempDir(), "kubeconfig")
	content := `apiVersion: v1
clusters:
- cluster:
    certificate-authority: /nonexistent/ca.crt
    server: https://127.0.0.1:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
kind: Config
users:
- name: test
  user: {}
`
	require.NoError(t, os.WriteFile(kcPath, []byte(content), 0o600))

	_, err := New(kcPath, "")
	assert.Error(t, err)
}

func TestRestConfigDifferentCertsDifferentData(t *testing.T) {
	kc1Path, _ := writeKubeconfig(t)
	kc2Path, _ := writeKubeconfig(t)

	kc1, err := New(kc1Path, "")
	require.NoError(t, err)
	kc2, err := New(kc2Path, "")
	require.NoError(t, err)

	cfg1, err := kc1.RestConfig()
	require.NoError(t, err)
	cfg2, err := kc2.RestConfig()
	require.NoError(t, err)

	assert.NotEqual(t, cfg1.CAData, cfg2.CAData,
		"independently generated CAs should produce different CAData")
}

func TestRestConfigAppliesServerOverride(t *testing.T) {
	kcPath, _ := writeKubeconfig(t)

	kc, err := New(kcPath, "https://10.0.0.1:6443")
	require.NoError(t, err)

	cfg, err := kc.RestConfig()
	require.NoError(t, err)

	assert.Equal(t, "https://10.0.0.1:6443", cfg.Host)
}

func TestRestConfigNoOverrideKeepsOriginal(t *testing.T) {
	kcPath, _ := writeKubeconfig(t)

	kc, err := New(kcPath, "")
	require.NoError(t, err)

	cfg, err := kc.RestConfig()
	require.NoError(t, err)

	assert.Equal(t, "https://127.0.0.1:6443", cfg.Host)
}

func TestWriteFlattenedInlinesData(t *testing.T) {
	kcPath, _ := writeKubeconfig(t)

	kc, err := New(kcPath, "")
	require.NoError(t, err)

	outPath := filepath.Join(t.TempDir(), "flattened")
	require.NoError(t, kc.WriteFlattened(outPath))

	reloaded, err := New(outPath, "")
	require.NoError(t, err)

	cfg, err := reloaded.RestConfig()
	require.NoError(t, err)

	assert.NotEmpty(t, cfg.CAData)
	assert.NotEmpty(t, cfg.CertData)
	assert.NotEmpty(t, cfg.KeyData)
	assert.Empty(t, cfg.CAFile)
	assert.Empty(t, cfg.CertFile)
	assert.Empty(t, cfg.KeyFile)
}
