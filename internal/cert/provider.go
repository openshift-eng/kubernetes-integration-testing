package cert

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	DefaultValidity = 10 * 365 * 24 * time.Hour
	DefaultCAName   = "cluster-ca"
	CASecretPrefix  = "kit-ca-"
	CALabel         = "kit.openshift.io/certificate-authority"
)

type Provider struct {
	mu        sync.Mutex
	cas       map[string]*CA
	leafCerts map[string]*LeafCert
	clientset kubernetes.Interface
	namespace string
	OnNewCA   func(name string, ca *CA)
}

func NewProvider() *Provider {
	return &Provider{
		cas:       make(map[string]*CA),
		leafCerts: make(map[string]*LeafCert),
	}
}

func NewPersistentProvider(clientset kubernetes.Interface, namespace string) *Provider {
	return &Provider{
		cas:       make(map[string]*CA),
		leafCerts: make(map[string]*LeafCert),
		clientset: clientset,
		namespace: namespace,
	}
}

func (p *Provider) SetCA(name string, ca *CA) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cas[name] = ca
}

func (p *Provider) GetCA(name string) (*CA, error) {
	return p.getOrCreateCA(name)
}

func (p *Provider) CACert(name string) (string, error) {
	ca, err := p.getOrCreateCA(name)
	if err != nil {
		return "", err
	}
	return ca.CertPEM, nil
}

func (p *Provider) SignedCert(caName, cn string) (string, error) {
	leaf, err := p.getOrCreateLeaf(caName, cn)
	if err != nil {
		return "", err
	}
	return leaf.CertPEM, nil
}

func (p *Provider) SignedKey(caName, cn string) (string, error) {
	leaf, err := p.getOrCreateLeaf(caName, cn)
	if err != nil {
		return "", err
	}
	return leaf.KeyPEM, nil
}

func (p *Provider) FuncMap() template.FuncMap {
	return template.FuncMap{
		"caCert":     p.CACert,
		"signedCert": p.SignedCert,
		"signedKey":  p.SignedKey,
	}
}

func CASecretName(name string) string {
	return CASecretPrefix + name
}

func leafSecretName(caName, cn string) string {
	h := sha256.Sum256([]byte(cn))
	return fmt.Sprintf("kit-leaf-%s-%x", caName, h[:6])
}

func (p *Provider) getOrCreateCA(name string) (*CA, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ca, ok := p.cas[name]; ok {
		return ca, nil
	}

	if p.clientset != nil {
		if ca, err := p.loadCAFromSecret(name); err != nil {
			return nil, err
		} else if ca != nil {
			p.cas[name] = ca
			return ca, nil
		}
	}

	ca, err := GenerateCA(name, DefaultValidity)
	if err != nil {
		return nil, err
	}

	if p.clientset != nil {
		if err := p.persistCASecret(name, ca); err != nil {
			return nil, err
		}
	}

	if p.OnNewCA != nil {
		p.OnNewCA(name, ca)
	}

	p.cas[name] = ca
	return ca, nil
}

func (p *Provider) loadCAFromSecret(name string) (*CA, error) {
	secretName := CASecretName(name)
	secret, err := p.clientset.CoreV1().Secrets(p.namespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading CA secret %s: %w", secretName, err)
	}
	ca, err := ParseCA(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, fmt.Errorf("parsing CA from secret %s: %w", secretName, err)
	}
	return ca, nil
}

func (p *Provider) persistCASecret(name string, ca *CA) error {
	secretName := CASecretName(name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: p.namespace,
			Labels:    map[string]string{CALabel: name},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(ca.CertPEM),
			corev1.TLSPrivateKeyKey: []byte(ca.KeyPEM),
		},
	}
	if _, err := p.clientset.CoreV1().Secrets(p.namespace).Create(context.TODO(), secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating CA secret %s: %w", secretName, err)
	}
	return nil
}

func (p *Provider) getOrCreateLeaf(caName, cn string) (*LeafCert, error) {
	key := caName + "/" + cn
	p.mu.Lock()
	if leaf, ok := p.leafCerts[key]; ok {
		p.mu.Unlock()
		return leaf, nil
	}
	p.mu.Unlock()

	if p.clientset != nil {
		if leaf, err := p.loadLeafFromSecret(caName, cn); err != nil {
			return nil, err
		} else if leaf != nil {
			p.mu.Lock()
			p.leafCerts[key] = leaf
			p.mu.Unlock()
			return leaf, nil
		}
	}

	ca, err := p.getOrCreateCA(caName)
	if err != nil {
		return nil, fmt.Errorf("getting CA %q: %w", caName, err)
	}

	leaf, err := GenerateLeafCert(ca, cn, []string{cn}, DefaultValidity)
	if err != nil {
		return nil, err
	}

	if p.clientset != nil {
		if err := p.persistLeafSecret(caName, cn, leaf); err != nil {
			return nil, err
		}
	}

	p.mu.Lock()
	p.leafCerts[key] = leaf
	p.mu.Unlock()
	return leaf, nil
}

func (p *Provider) loadLeafFromSecret(caName, cn string) (*LeafCert, error) {
	secretName := leafSecretName(caName, cn)
	secret, err := p.clientset.CoreV1().Secrets(p.namespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading leaf secret %s: %w", secretName, err)
	}
	return &LeafCert{
		CertPEM: string(secret.Data[corev1.TLSCertKey]),
		KeyPEM:  string(secret.Data[corev1.TLSPrivateKeyKey]),
	}, nil
}

func (p *Provider) persistLeafSecret(caName, cn string, leaf *LeafCert) error {
	secretName := leafSecretName(caName, cn)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: p.namespace,
			Labels:    map[string]string{CALabel: caName},
		},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(leaf.CertPEM),
			corev1.TLSPrivateKeyKey: []byte(leaf.KeyPEM),
		},
	}
	if _, err := p.clientset.CoreV1().Secrets(p.namespace).Create(context.TODO(), secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating leaf secret %s: %w", secretName, err)
	}
	return nil
}
