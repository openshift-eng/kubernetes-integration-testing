package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/cert"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	WebhookConfigName = "kit-workload-overrides"
	WebhookName       = "workload-overrides.kit.openshift.io"
	WebhookPort       = 9443

	startupTimeout = 30 * time.Second
)

// Configurer prepares the mutating webhook server and manages the
// MutatingWebhookConfiguration lifecycle.
type Configurer struct {
	containerName string
	caBundle      []byte
	srv           webhook.Server
	client        client.Client
	log           *slog.Logger
	ready         atomic.Bool
}

// NewConfigurer creates the webhook server, registers the pod mutator handler,
// and adds the server to the manager.
func NewConfigurer(mgr ctrl.Manager, clients *kube.Clients, containerName string, certProvider *cert.Provider, log *slog.Logger) (*Configurer, error) {
	certDir, caBundle, err := writeTLS(certProvider, containerName, log)
	if err != nil {
		return nil, err
	}

	srv := webhook.NewServer(webhook.Options{
		Port:    WebhookPort,
		CertDir: certDir,
	})
	if err := mgr.Add(srv); err != nil {
		return nil, fmt.Errorf("adding webhook server to manager: %w", err)
	}

	srv.Register(PausePath, &webhook.Admission{
		Handler: &PodMutator{
			Client:  clients.Client,
			Decoder: admission.NewDecoder(kube.Scheme),
			Log:     log,
		},
	})

	return &Configurer{
		containerName: containerName,
		caBundle:      caBundle,
		srv:           srv,
		client:        clients.Client,
		log:           log,
	}, nil
}

// EnsureConfiguration creates or updates the MutatingWebhookConfiguration.
// Call before mgr.Start so that FailurePolicy=Fail blocks pod admission
// until the webhook server is serving.
func (c *Configurer) EnsureConfiguration(ctx context.Context) error {
	if err := c.ensureConfiguration(ctx); err != nil {
		return err
	}
	c.log.Info("ensured MutatingWebhookConfiguration")
	return nil
}

// WaitReady blocks until the webhook server is accepting connections.
// Call after mgr.Start.
func (c *Configurer) WaitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	checker := c.srv.StartedChecker()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := checker(nil); err == nil {
			c.ready.Store(true)
			c.log.Info("webhook server is ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("webhook server did not start within %s", startupTimeout)
		case <-ticker.C:
		}
	}
}

// Ready implements healthz.Checker for the manager's readiness probe.
func (c *Configurer) Ready(_ *http.Request) error {
	if !c.ready.Load() {
		return fmt.Errorf("workload override webhook is not installed")
	}
	return nil
}

func (c *Configurer) ensureConfiguration(ctx context.Context) error {
	url := fmt.Sprintf("https://%s:%d%s", c.containerName, WebhookPort, PausePath)
	fail := admissionregistrationv1.Fail
	none := admissionregistrationv1.SideEffectClassNone
	never := admissionregistrationv1.NeverReinvocationPolicy

	desired := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: WebhookConfigName,
		},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{
				Name:                    WebhookName,
				AdmissionReviewVersions: []string{"v1"},
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					URL:      &url,
					CABundle: c.caBundle,
				},
				Rules: []admissionregistrationv1.RuleWithOperations{
					{
						Operations: []admissionregistrationv1.OperationType{
							admissionregistrationv1.Create,
						},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{""},
							APIVersions: []string{"v1"},
							Resources:   []string{"pods"},
							Scope:       ptr.To(admissionregistrationv1.NamespacedScope),
						},
					},
				},
				FailurePolicy:      &fail,
				SideEffects:        &none,
				ReinvocationPolicy: &never,
			},
		},
	}

	var existing admissionregistrationv1.MutatingWebhookConfiguration
	if err := c.client.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return c.client.Create(ctx, desired)
		}
		return fmt.Errorf("getting webhook configuration: %w", err)
	}

	existing.Webhooks = desired.Webhooks
	return c.client.Update(ctx, &existing)
}

// writeTLS writes the webhook serving cert/key to a temp dir.
// The provider caches certs by CN, so repeated calls reuse the same leaf.
func writeTLS(certProvider *cert.Provider, containerName string, log *slog.Logger) (string, []byte, error) {
	caName := cert.DefaultCAName
	caCertPEM, err := certProvider.CACert(caName)
	if err != nil {
		return "", nil, fmt.Errorf("getting CA cert: %w", err)
	}

	certPEM, err := certProvider.SignedCert(caName, containerName, containerName)
	if err != nil {
		return "", nil, fmt.Errorf("issuing webhook cert: %w", err)
	}
	keyPEM, err := certProvider.SignedKey(caName, containerName, containerName)
	if err != nil {
		return "", nil, fmt.Errorf("getting webhook key: %w", err)
	}

	dir, err := os.MkdirTemp("", "kit-webhook-tls-")
	if err != nil {
		return "", nil, fmt.Errorf("creating cert dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte(certPEM), 0o600); err != nil {
		return "", nil, fmt.Errorf("writing webhook cert: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte(keyPEM), 0o600); err != nil {
		return "", nil, fmt.Errorf("writing webhook key: %w", err)
	}

	log.Info("prepared webhook TLS", "certDir", dir, "cn", containerName)
	return dir, []byte(caCertPEM), nil
}
