package cert

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.getOrCreateCA(name)
}

func (p *Provider) CACert(name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ca, err := p.getOrCreateCA(name)
	if err != nil {
		return "", err
	}
	return ca.CertPEM, nil
}

func (p *Provider) SignedCert(caName, cn string, dnsNames ...string) (string, error) {
	leaf, err := p.getOrCreateLeaf(caName, cn, dnsNames...)
	if err != nil {
		return "", err
	}
	return leaf.CertPEM, nil
}

func (p *Provider) SignedKey(caName, cn string, dnsNames ...string) (string, error) {
	leaf, err := p.getOrCreateLeaf(caName, cn, dnsNames...)
	if err != nil {
		return "", err
	}
	return leaf.KeyPEM, nil
}

func CASecretName(name string) string {
	return CASecretPrefix + name
}

func leafSecretName(caName, cn string) string {
	h := sha256.Sum256([]byte(cn))
	return fmt.Sprintf("kit-leaf-%s-%x", caName, h[:6])
}

func (p *Provider) getOrCreateCA(name string) (*CA, error) {
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
		ca, err = p.persistOrLoadCA(name, ca)
		if err != nil {
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

func (p *Provider) persistOrLoadCA(name string, ca *CA) (*CA, error) {
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
	_, err := p.clientset.CoreV1().Secrets(p.namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if err == nil {
		return ca, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating CA secret %s: %w", secretName, err)
	}
	existing, loadErr := p.loadCAFromSecret(name)
	if loadErr != nil {
		return nil, loadErr
	}
	return existing, nil
}

func (p *Provider) getOrCreateLeaf(caName, cn string, dnsNames ...string) (*LeafCert, error) {
	key := caName + "/" + cn
	p.mu.Lock()
	defer p.mu.Unlock()

	if leaf, ok := p.leafCerts[key]; ok {
		return leaf, nil
	}

	if p.clientset != nil {
		if leaf, err := p.loadLeafFromSecret(caName, cn); err != nil {
			return nil, err
		} else if leaf != nil {
			p.leafCerts[key] = leaf
			return leaf, nil
		}
	}

	ca, err := p.getOrCreateCA(caName)
	if err != nil {
		return nil, fmt.Errorf("getting CA %q: %w", caName, err)
	}

	sans := dnsNames
	if len(sans) == 0 {
		sans = []string{cn}
	}
	leaf, err := GenerateLeafCert(ca, cn, sans, DefaultValidity)
	if err != nil {
		return nil, err
	}

	if p.clientset != nil {
		leaf, err = p.persistOrLoadLeaf(caName, cn, leaf)
		if err != nil {
			return nil, err
		}
	}

	p.leafCerts[key] = leaf
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

func (p *Provider) persistOrLoadLeaf(caName, cn string, leaf *LeafCert) (*LeafCert, error) {
	secretName := leafSecretName(caName, cn)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: p.namespace,
			Labels:    map[string]string{CALabel: caName},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(leaf.CertPEM),
			corev1.TLSPrivateKeyKey: []byte(leaf.KeyPEM),
		},
	}
	_, err := p.clientset.CoreV1().Secrets(p.namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if err == nil {
		return leaf, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating leaf secret %s: %w", secretName, err)
	}
	existing, loadErr := p.loadLeafFromSecret(caName, cn)
	if loadErr != nil {
		return nil, loadErr
	}
	return existing, nil
}
