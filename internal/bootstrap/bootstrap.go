package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/cert"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kube"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	Namespace     = "kit-system"
	configMapName = "cluster-config"
	manifestKey   = "manifest"
	configKey     = "config.json"
	secretName    = "pull-secret"
	secretKey     = ".dockerconfigjson"
)

// ClusterConfig holds infrastructure plumbing that the controller needs
// to operate but that does not belong on the KitCluster CR.
type ClusterConfig struct {
	Network       string `json:"network,omitempty"`
	LogsDir       string `json:"logsDir,omitempty"`
	VolumesDir    string `json:"volumesDir,omitempty"`
	APIServerHost string `json:"apiServerHost,omitempty"`
	APIServerPort string `json:"apiServerPort,omitempty"`
}

func Store(ctx context.Context, kc *kube.KubeConfig, cfg ClusterConfig, kitCluster *v1alpha1.KitCluster, pullSecret, caCertPEM, caKeyPEM []byte) error {
	clientset, err := newClientset(kc)
	if err != nil {
		return err
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating namespace %s: %w", Namespace, err)
	}

	cfgData, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling cluster config: %w", err)
	}
	manifestData, err := json.Marshal(kitCluster)
	if err != nil {
		return fmt.Errorf("marshaling KitCluster manifest: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: Namespace},
		Data: map[string]string{
			configKey:   string(cfgData),
			manifestKey: string(manifestData),
		},
	}
	if err := upsertConfigMap(ctx, clientset, cm); err != nil {
		return fmt.Errorf("storing cluster config: %w", err)
	}

	if len(pullSecret) > 0 {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: Namespace},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{secretKey: pullSecret},
		}
		if err := upsertSecret(ctx, clientset, secret); err != nil {
			return fmt.Errorf("storing pull secret: %w", err)
		}
	}

	if len(caCertPEM) > 0 && len(caKeyPEM) > 0 {
		caName := kitCluster.Spec.DefaultCA
		if caName == "" {
			caName = cert.DefaultCAName
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cert.CASecretName(caName),
				Namespace: Namespace,
				Labels:    map[string]string{cert.CALabel: caName},
			},
			Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{
				corev1.TLSCertKey:       caCertPEM,
				corev1.TLSPrivateKeyKey: caKeyPEM,
			},
		}
		if err := upsertSecret(ctx, clientset, secret); err != nil {
			return fmt.Errorf("storing CA keypair: %w", err)
		}
	}

	return nil
}

type BootstrapData struct {
	ClusterConfig *ClusterConfig
	KitCluster    *v1alpha1.KitCluster
	PullSecret    []byte
}

func Load(ctx context.Context, kubeConfig *kube.KubeConfig) (*BootstrapData, error) {
	clientset, err := newClientset(kubeConfig)
	if err != nil {
		return nil, err
	}

	cm, err := clientset.CoreV1().ConfigMaps(Namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("loading cluster config: %w", err)
	}

	var cfg ClusterConfig
	if err := json.Unmarshal([]byte(cm.Data[configKey]), &cfg); err != nil {
		return nil, fmt.Errorf("parsing cluster config: %w", err)
	}

	var kc v1alpha1.KitCluster
	if err := json.Unmarshal([]byte(cm.Data[manifestKey]), &kc); err != nil {
		return nil, fmt.Errorf("parsing KitCluster manifest: %w", err)
	}

	data := &BootstrapData{ClusterConfig: &cfg, KitCluster: &kc}

	secret, err := clientset.CoreV1().Secrets(Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		data.PullSecret = secret.Data[secretKey]
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("loading pull secret: %w", err)
	}

	return data, nil
}

func newClientset(kc *kube.KubeConfig) (*kubernetes.Clientset, error) {
	cfg, err := kc.RestConfig()
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return cs, nil
}

func upsertConfigMap(ctx context.Context, cs *kubernetes.Clientset, cm *corev1.ConfigMap) error {
	existing, err := cs.CoreV1().ConfigMaps(cm.Namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cs.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	cm.ResourceVersion = existing.ResourceVersion
	_, err = cs.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func upsertSecret(ctx context.Context, cs *kubernetes.Clientset, secret *corev1.Secret) error {
	existing, err := cs.CoreV1().Secrets(secret.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cs.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	secret.ResourceVersion = existing.ResourceVersion
	_, err = cs.CoreV1().Secrets(secret.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
	return err
}
