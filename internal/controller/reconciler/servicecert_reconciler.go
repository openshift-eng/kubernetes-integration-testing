package reconciler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/cert"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

	cn := fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace)
	certPEM, err := r.certProvider.SignedCert(cert.DefaultCAName, cn)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generating cert for service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	keyPEM, err := r.certProvider.SignedKey(cert.DefaultCAName, cn)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generating key for service %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: svc.Namespace,
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

func (r *serviceReconciler) SetupWithManager(mgr ctrl.Manager, clusterName string) error {
	hasAnnotation := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		_, ok := obj.GetAnnotations()[servingCertAnnotation]
		return ok
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("servicecert-"+clusterName).
		For(&corev1.Service{}, builder.WithPredicates(hasAnnotation)).
		Complete(r)
}

var _ reconcile.Reconciler = &serviceReconciler{}
