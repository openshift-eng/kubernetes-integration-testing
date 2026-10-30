package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/bootstrap"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/cert"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const caFinalizer = "kit.openshift.io/ca-protection"

type caReconciler struct {
	client       client.Client
	clientset    kubernetes.Interface
	certProvider *cert.Provider
	clusterName  string
	log          *slog.Logger
}

func (r *caReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ca v1alpha1.CertificateAuthority
	if err := r.client.Get(ctx, req.NamespacedName, &ca); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ca.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ca)
	}

	if !controllerutil.ContainsFinalizer(&ca, caFinalizer) {
		controllerutil.AddFinalizer(&ca, caFinalizer)
		if err := r.client.Update(ctx, &ca); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	secretName := cert.CASecretName(ca.Name)
	secret, err := r.clientset.CoreV1().Secrets(bootstrap.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return r.provisionCA(ctx, &ca, secretName)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting CA secret: %w", err)
	}

	parsed, err := cert.ParseCA(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("parsing CA from secret: %w", err)
	}

	if ca.Spec.CommonName != parsed.Cert.Subject.CommonName {
		r.log.Warn("CA secret CN does not match spec",
			"ca", ca.Name, "specCN", ca.Spec.CommonName, "certCN", parsed.Cert.Subject.CommonName)
		return ctrl.Result{}, r.setNotReady(ctx, &ca, "CNMismatch",
			fmt.Sprintf("secret CN %q does not match spec CN %q", parsed.Cert.Subject.CommonName, ca.Spec.CommonName))
	}

	r.certProvider.SetCA(ca.Name, parsed)
	return ctrl.Result{}, r.setReady(ctx, &ca, secretName, parsed.Cert.NotAfter)
}

func (r *caReconciler) provisionCA(ctx context.Context, ca *v1alpha1.CertificateAuthority, secretName string) (ctrl.Result, error) {
	validity := ca.Spec.Validity.Duration
	if validity == 0 {
		validity = cert.DefaultValidity
	}

	generated, err := cert.GenerateCA(ca.Spec.CommonName, validity)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generating CA %q: %w", ca.Name, err)
	}
	r.certProvider.SetCA(ca.Name, generated)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: bootstrap.Namespace,
			Labels:    map[string]string{cert.CALabel: ca.Name},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(generated.CertPEM),
			corev1.TLSPrivateKeyKey: []byte(generated.KeyPEM),
		},
	}
	if _, err := r.clientset.CoreV1().Secrets(bootstrap.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("creating CA secret: %w", err)
	}

	r.log.Info("created CA secret", "name", secretName, "ca", ca.Name)
	return ctrl.Result{}, r.setReady(ctx, ca, secretName, generated.Cert.NotAfter)
}

func (r *caReconciler) setNotReady(ctx context.Context, ca *v1alpha1.CertificateAuthority, reason, message string) error {
	base := ca.DeepCopy()
	meta.SetStatusCondition(&ca.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionCAReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ca.Generation,
	})
	return r.client.Status().Patch(ctx, ca, client.MergeFrom(base))
}

func (r *caReconciler) setReady(ctx context.Context, ca *v1alpha1.CertificateAuthority, secretName string, notAfter time.Time) error {
	base := ca.DeepCopy()
	ca.Status.SecretRef = &corev1.SecretReference{Name: secretName, Namespace: bootstrap.Namespace}
	ca.Status.NotAfter = metav1.NewTime(notAfter)
	ca.Status.SignedCertificates = r.listSignedCerts(ctx, ca.Name, secretName)
	meta.SetStatusCondition(&ca.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionCAReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Provisioned",
		ObservedGeneration: ca.Generation,
	})
	return r.client.Status().Patch(ctx, ca, client.MergeFrom(base))
}

func (r *caReconciler) listSignedCerts(ctx context.Context, caName, caSecretName string) []v1alpha1.SignedCertificateRef {
	secrets, err := r.clientset.CoreV1().Secrets(bootstrap.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: cert.CALabel + "=" + caName,
	})
	if err != nil {
		r.log.Error("listing signed certificates", "ca", caName, "error", err)
		return nil
	}

	var refs []v1alpha1.SignedCertificateRef
	for _, s := range secrets.Items {
		if s.Name == caSecretName {
			continue
		}
		cn := cert.CommonNameFromPEM(s.Data[corev1.TLSCertKey])
		refs = append(refs, v1alpha1.SignedCertificateRef{
			SecretReference: corev1.SecretReference{Name: s.Name, Namespace: s.Namespace},
			CommonName:      cn,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

func (r *caReconciler) handleDeletion(ctx context.Context, ca *v1alpha1.CertificateAuthority) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ca, caFinalizer) {
		return ctrl.Result{}, nil
	}

	if isDefault, err := r.isDefaultCA(ctx, ca.Name); err != nil {
		return ctrl.Result{}, fmt.Errorf("checking if CA is default: %w", err)
	} else if isDefault {
		r.log.Info("blocking deletion of default CA", "ca", ca.Name)
		return ctrl.Result{}, nil
	}

	secrets, err := r.clientset.CoreV1().Secrets(bootstrap.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: cert.CALabel + "=" + ca.Name,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing secrets for CA %q: %w", ca.Name, err)
	}
	for _, s := range secrets.Items {
		if err := r.clientset.CoreV1().Secrets(bootstrap.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting secret %s: %w", s.Name, err)
		}
		r.log.Info("deleted secret", "name", s.Name, "ca", ca.Name)
	}

	controllerutil.RemoveFinalizer(ca, caFinalizer)
	if err := r.client.Update(ctx, ca); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *caReconciler) isDefaultCA(ctx context.Context, name string) (bool, error) {
	var kc v1alpha1.KitCluster
	if err := r.client.Get(ctx, client.ObjectKey{Name: r.clusterName}, &kc); err != nil {
		return false, err
	}
	return kc.Status.DefaultCA == name, nil
}

func (r *caReconciler) secretToCA(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Namespace != bootstrap.Namespace {
		return nil
	}
	caName, ok := secret.Labels[cert.CALabel]
	if !ok || caName == "" {
		return nil
	}

	var ca v1alpha1.CertificateAuthority
	if err := r.client.Get(ctx, client.ObjectKey{Name: caName}, &ca); err == nil {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: caName}}}
	}

	cr := &v1alpha1.CertificateAuthority{
		ObjectMeta: metav1.ObjectMeta{Name: caName},
	}
	if parsed, err := cert.ParseCA(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]); err == nil {
		cr.Spec.CommonName = parsed.Cert.Subject.CommonName
		cr.Spec.Validity = metav1.Duration{Duration: parsed.Cert.NotAfter.Sub(parsed.Cert.NotBefore)}
	}
	if err := r.client.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
		r.log.Error("auto-creating CertificateAuthority CR from secret", "ca", caName, "error", err)
		return nil
	}
	r.log.Info("auto-created CertificateAuthority CR from secret", "ca", caName)
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: caName}}}
}

func (r *caReconciler) SetupWithManager(mgr ctrl.Manager, clusterName string) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("certificateauthority-"+clusterName).
		For(&v1alpha1.CertificateAuthority{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToCA)).
		Complete(r)
}
