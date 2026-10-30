package reconciler

import (
	"context"
	"fmt"
	"log/slog"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/cert"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const servingCertAnnotation = "service.beta.openshift.io/serving-cert-secret-name"

type serviceReconciler struct {
	client       client.Client
	clientset    kubernetes.Interface
	certProvider *cert.Provider
	clusterName  string
	log          *slog.Logger
}

func (r *serviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var svc corev1.Service
	if err := r.client.Get(ctx, req.NamespacedName, &svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	secretName, ok := svc.Annotations[servingCertAnnotation]
	if !ok || secretName == "" {
		return ctrl.Result{}, nil
	}

	caName, ready := r.resolveCA(ctx)
	if !ready {
		r.log.Info("CA not ready, requeueing", "ca", caName, "service", svc.Name, "namespace", svc.Namespace)
		return ctrl.Result{Requeue: true}, nil
	}

	cn := fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace)
	dnsNames := serviceDNSNames(&svc)
	certPEM, err := r.certProvider.SignedCert(caName, cn, dnsNames...)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generating cert for service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	keyPEM, err := r.certProvider.SignedKey(caName, cn, dnsNames...)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generating key for service %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: svc.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Service",
				Name:       svc.Name,
				UID:        svc.UID,
				Controller: ptr.To(true),
			}},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(certPEM),
			corev1.TLSPrivateKeyKey: []byte(keyPEM),
		},
	}

	if _, err := r.clientset.CoreV1().Secrets(svc.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating secret %s/%s: %w", svc.Namespace, secretName, err)
	}

	r.log.Info("provisioned serving cert", "namespace", svc.Namespace, "service", svc.Name, "secret", secretName)
	return ctrl.Result{}, nil
}

func (r *serviceReconciler) resolveCA(ctx context.Context) (string, bool) {
	var kc v1alpha1.KitCluster
	if err := r.client.Get(ctx, client.ObjectKey{Name: r.clusterName}, &kc); err != nil {
		return cert.DefaultCAName, false
	}
	caName := kc.Status.DefaultCA
	if caName == "" {
		caName = cert.DefaultCAName
	}
	var ca v1alpha1.CertificateAuthority
	if err := r.client.Get(ctx, client.ObjectKey{Name: caName}, &ca); err != nil {
		return caName, false
	}
	return caName, meta.IsStatusConditionTrue(ca.Status.Conditions, v1alpha1.ConditionCAReady)
}

// serviceDNSNames replicates the SAN logic from openshift/service-ca-operator's
// certSubjectsForService: standard FQDN SANs plus pod wildcards for headless services.
func serviceDNSNames(svc *corev1.Service) []string {
	hostname := svc.Name + "." + svc.Namespace + ".svc"
	names := []string{hostname, hostname + ".cluster.local"}
	if svc.Spec.ClusterIP == corev1.ClusterIPNone {
		wildcard := "*." + svc.Name + "." + svc.Namespace + ".svc"
		names = append(names, wildcard, wildcard+".cluster.local")
	}
	return names
}

func (r *serviceReconciler) SetupWithManager(mgr ctrl.Manager, clusterName string) error {
	hasAnnotation := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		_, ok := obj.GetAnnotations()[servingCertAnnotation]
		return ok
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("servicecert-"+clusterName).
		For(&corev1.Service{}, builder.WithPredicates(hasAnnotation)).
		Owns(&corev1.Secret{}).
		Complete(r)
}

var _ reconcile.Reconciler = &serviceReconciler{}
