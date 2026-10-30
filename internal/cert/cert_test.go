package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateCA(t *testing.T) {
	ca, err := GenerateCA("test-ca", 24*time.Hour)
	require.NoError(t, err)

	assert.Equal(t, "test-ca", ca.Cert.Subject.CommonName)
	assert.True(t, ca.Cert.IsCA)
	assert.NotEmpty(t, ca.CertPEM)
	assert.NotEmpty(t, ca.KeyPEM)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), ca.Cert.NotAfter, 2*time.Minute)
}

func TestParseCA(t *testing.T) {
	original, err := GenerateCA("roundtrip-ca", 24*time.Hour)
	require.NoError(t, err)

	parsed, err := ParseCA([]byte(original.CertPEM), []byte(original.KeyPEM))
	require.NoError(t, err)

	assert.Equal(t, original.Cert.Subject.CommonName, parsed.Cert.Subject.CommonName)
	assert.True(t, parsed.Cert.IsCA)
	assert.Equal(t, original.KeyPEM, parsed.KeyPEM)
}

func TestParseCARSAKey(t *testing.T) {
	// Simulate what kwok/kind produce: RSA key in PKCS1 format
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "rsa-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rsaKey.PublicKey, rsaKey)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})

	ca, err := ParseCA(certPEM, keyPEM)
	require.NoError(t, err)
	assert.Equal(t, "rsa-ca", ca.Cert.Subject.CommonName)
	assert.True(t, ca.Cert.IsCA)
	_, ok := ca.Key.(*rsa.PrivateKey)
	assert.True(t, ok, "key should be *rsa.PrivateKey")
}

func TestParseCAPKCS8Key(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pkcs8-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &ecKey.PublicKey, ecKey)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	ca, err := ParseCA(certPEM, keyPEM)
	require.NoError(t, err)
	assert.Equal(t, "pkcs8-ca", ca.Cert.Subject.CommonName)
	_, ok := ca.Key.(*ecdsa.PrivateKey)
	assert.True(t, ok, "PKCS8-wrapped EC key should unwrap to *ecdsa.PrivateKey")
}

func TestGenerateLeafCertWithRSACA(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "rsa-leaf-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rsaKey.PublicKey, rsaKey)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})

	ca, err := ParseCA(certPEM, keyPEM)
	require.NoError(t, err)

	leaf, err := GenerateLeafCert(ca, "svc.ns.svc", []string{"svc.ns.svc"}, 24*time.Hour)
	require.NoError(t, err)

	block, _ := pem.Decode([]byte(leaf.CertPEM))
	require.NotNil(t, block)
	leafCert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	_, err = leafCert.Verify(x509.VerifyOptions{Roots: roots})
	require.NoError(t, err, "leaf cert signed by RSA CA should verify")
}

func TestCommonNameFromPEM(t *testing.T) {
	ca, err := GenerateCA("cn-extract-test", 24*time.Hour)
	require.NoError(t, err)

	assert.Equal(t, "cn-extract-test", CommonNameFromPEM([]byte(ca.CertPEM)))
	assert.Equal(t, "", CommonNameFromPEM([]byte("not-a-pem")))
	assert.Equal(t, "", CommonNameFromPEM(nil))
}

func TestParseCAInvalidCert(t *testing.T) {
	_, err := ParseCA([]byte("not-a-pem"), []byte("not-a-pem"))
	assert.Error(t, err)
}

func TestGenerateLeafCert(t *testing.T) {
	ca, err := GenerateCA("leaf-test-ca", 24*time.Hour)
	require.NoError(t, err)

	dnsNames := []string{
		"my-service",
		"my-service.ns",
		"my-service.ns.svc",
		"my-service.ns.svc.cluster.local",
	}
	leaf, err := GenerateLeafCert(ca, "my-service.ns.svc", dnsNames, 24*time.Hour)
	require.NoError(t, err)

	assert.NotEmpty(t, leaf.CertPEM)
	assert.NotEmpty(t, leaf.KeyPEM)

	block, _ := pem.Decode([]byte(leaf.CertPEM))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	assert.Equal(t, "my-service.ns.svc", cert.Subject.CommonName)
	assert.Equal(t, dnsNames, cert.DNSNames)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), cert.NotAfter, 2*time.Minute)

	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots})
	require.NoError(t, err)
}
