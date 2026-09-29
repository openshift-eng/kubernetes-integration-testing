package deployer

// HACK(PoC): SecretProvisioner watches Deployments, DaemonSets and
// StatefulSets for secret volume references and generates self-signed TLS
// secrets for any that don't already exist. This is a brute-force workaround
// that blindly assumes every missing secret volume expects TLS data
// (tls.crt / tls.key). It exists solely to unblock the PoC — some operators
// (e.g. the MCO) create workloads at runtime that reference secrets normally
// provisioned by the installer bootstrap, and nothing else creates them.
//
// TODO: replace this with a proper, operator-independent mechanism for
// discovering and provisioning required secrets. Possible approaches:
//   - Let operators declare their secret requirements via annotations
//   - Use admission webhooks to intercept and auto-provision
//   - Provide a manifest-level "seed secrets" configuration

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

type SecretProvisioner struct {
	clientset   kubernetes.Interface
	ca          *servingCertCA
	log         *slog.Logger
	mu          sync.Mutex
	provisioned map[string]bool
}

func NewSecretProvisioner(kubeconfig string, log *slog.Logger) (*SecretProvisioner, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	ca, err := generateServingCA()
	if err != nil {
		return nil, fmt.Errorf("generating serving CA: %w", err)
	}

	return &SecretProvisioner{
		clientset:   clientset,
		ca:          ca,
		log:         log,
		provisioned: make(map[string]bool),
	}, nil
}

func (p *SecretProvisioner) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(p.clientset, 0)

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			p.handleWorkload(ctx, obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			p.handleWorkload(ctx, newObj)
		},
	}

	_, _ = factory.Apps().V1().Deployments().Informer().AddEventHandler(handler)
	_, _ = factory.Apps().V1().DaemonSets().Informer().AddEventHandler(handler)
	_, _ = factory.Apps().V1().StatefulSets().Informer().AddEventHandler(handler)

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	<-ctx.Done()
	return nil
}

func (p *SecretProvisioner) handleWorkload(ctx context.Context, obj interface{}) {
	var volumes []corev1.Volume
	var namespace string

	switch w := obj.(type) {
	case *appsv1.Deployment:
		volumes = w.Spec.Template.Spec.Volumes
		namespace = w.Namespace
	case *appsv1.DaemonSet:
		volumes = w.Spec.Template.Spec.Volumes
		namespace = w.Namespace
	case *appsv1.StatefulSet:
		volumes = w.Spec.Template.Spec.Volumes
		namespace = w.Namespace
	default:
		return
	}

	for _, vol := range volumes {
		if vol.Secret == nil {
			continue
		}
		if vol.Secret.Optional != nil && *vol.Secret.Optional {
			continue
		}
		p.ensureSecret(ctx, namespace, vol.Secret.SecretName)
	}
}

func (p *SecretProvisioner) ensureSecret(ctx context.Context, namespace, name string) {
	key := namespace + "/" + name

	p.mu.Lock()
	if p.provisioned[key] {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	_, err := p.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		p.mu.Lock()
		p.provisioned[key] = true
		p.mu.Unlock()
		return
	}
	if !apierrors.IsNotFound(err) {
		return
	}

	certB64, keyB64, err := generateServingCert(p.ca, name, namespace)
	if err != nil {
		p.log.Warn("failed to generate cert for secret", "namespace", namespace, "name", name, "error", err)
		return
	}

	certBytes, _ := base64.StdEncoding.DecodeString(certB64)
	keyBytes, _ := base64.StdEncoding.DecodeString(keyB64)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certBytes,
			"tls.key": keyBytes,
		},
	}

	if _, err := p.clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			p.log.Warn("failed to create secret", "namespace", namespace, "name", name, "error", err)
		}
		return
	}

	p.mu.Lock()
	p.provisioned[key] = true
	p.mu.Unlock()

	p.log.Info("provisioned TLS secret for volume reference", "namespace", namespace, "name", name)
}
