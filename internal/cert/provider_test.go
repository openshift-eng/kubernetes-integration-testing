package cert

import (
	"bytes"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderCACertAutoGenerates(t *testing.T) {
	p := NewProvider()

	pem1, err := p.CACert("test-ca")
	require.NoError(t, err)
	assert.Contains(t, pem1, "BEGIN CERTIFICATE")

	pem2, err := p.CACert("test-ca")
	require.NoError(t, err)
	assert.Equal(t, pem1, pem2, "same CA name should return same cert")
}

func TestProviderSetCA(t *testing.T) {
	p := NewProvider()

	ca, err := GenerateCA("preset", DefaultValidity)
	require.NoError(t, err)
	p.SetCA("cluster-ca", ca)

	pem, err := p.CACert("cluster-ca")
	require.NoError(t, err)
	assert.Equal(t, ca.CertPEM, pem)
}

func TestProviderSignedCertAndKey(t *testing.T) {
	p := NewProvider()

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

func TestProviderFuncMapInTemplate(t *testing.T) {
	p := NewProvider()

	ca, err := GenerateCA("cluster-ca", DefaultValidity)
	require.NoError(t, err)
	p.SetCA("cluster-ca", ca)

	src := `ca: {{ caCert "cluster-ca" }}`
	tmpl, err := template.New("test").Funcs(p.FuncMap()).Parse(src)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, tmpl.Execute(&buf, nil))
	assert.Contains(t, buf.String(), "BEGIN CERTIFICATE")
}
