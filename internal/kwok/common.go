package kwok

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type VolumeNotReadyError struct {
	Missing []string
}

func (e *VolumeNotReadyError) Error() string {
	return fmt.Sprintf("volumes not ready: %s", strings.Join(e.Missing, ", "))
}

func processKey(podUID, container string) string {
	return podUID + "/" + container
}

func containerLogPath(logsDir, namespace, pod, container string) string {
	return filepath.Join(logsDir, namespace, pod, container+".log")
}

func materializeVolumes(ctx context.Context, clientset kubernetes.Interface, volumesDir string, pod *corev1.Pod, container corev1.Container, log *slog.Logger) (map[string]string, error) {
	volumeMap := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		volumeMap[v.Name] = v
	}

	rewrites := make(map[string]string)
	var firstErr error
	var missing []string
	for _, vm := range container.VolumeMounts {
		vol, ok := volumeMap[vm.Name]
		if !ok {
			continue
		}

		localDir := filepath.Join(volumesDir, pod.Namespace, pod.Name, vm.Name)
		if err := os.MkdirAll(localDir, 0o755); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		var data map[string][]byte
		switch {
		case vol.ConfigMap != nil:
			cm, err := clientset.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, vol.ConfigMap.Name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) && (vol.ConfigMap.Optional == nil || !*vol.ConfigMap.Optional) {
					missing = append(missing, "configmap/"+vol.ConfigMap.Name)
					continue
				}
				log.Warn("failed to get configmap for volume", "configmap", vol.ConfigMap.Name, "volume", vm.Name, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			data = make(map[string][]byte, len(cm.Data)+len(cm.BinaryData))
			for k, v := range cm.Data {
				data[k] = []byte(v)
			}
			for k, v := range cm.BinaryData {
				data[k] = v
			}
		case vol.Secret != nil:
			secret, err := clientset.CoreV1().Secrets(pod.Namespace).Get(ctx, vol.Secret.SecretName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) && (vol.Secret.Optional == nil || !*vol.Secret.Optional) {
					missing = append(missing, "secret/"+vol.Secret.SecretName)
					continue
				}
				log.Warn("failed to get secret for volume", "secret", vol.Secret.SecretName, "volume", vm.Name, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			data = secret.Data
		default:
			continue
		}

		for filename, content := range data {
			if err := os.WriteFile(filepath.Join(localDir, filename), content, 0o644); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		rewrites[vm.MountPath] = localDir
	}

	if len(missing) > 0 {
		return rewrites, &VolumeNotReadyError{Missing: missing}
	}
	return rewrites, firstErr
}

const volumeWaitTimeout = 2 * time.Minute

// VolumeWaitFunc is called each time waitForVolumes detects missing volumes,
// allowing the caller to emit events or update status during the wait.
type VolumeWaitFunc func(missing []string)

func waitForVolumes(ctx context.Context, clientset kubernetes.Interface, volumesDir string, pod *corev1.Pod, container corev1.Container, log *slog.Logger, onWait VolumeWaitFunc) (map[string]string, error) {
	deadline := time.Now().Add(volumeWaitTimeout)
	attempt := 0
	for {
		mounts, err := materializeVolumes(ctx, clientset, volumesDir, pod, container, log)
		if err == nil {
			return mounts, nil
		}
		var notReady *VolumeNotReadyError
		if !errors.As(err, &notReady) {
			return mounts, err
		}
		if onWait != nil {
			onWait(notReady.Missing)
		}
		if time.Now().After(deadline) {
			return mounts, err
		}
		attempt++
		backoff := volumeBackoff(attempt)
		log.Info("waiting for volumes", "pod", pod.Name, "namespace", pod.Namespace, "missing", notReady.Missing, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func volumeBackoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// volumeEventEmitter returns a VolumeWaitFunc that emits FailedMount events
// on the pod, emulating the kubelet's behavior during volume setup retries.
func volumeEventEmitter(clientset kubernetes.Interface, pod *corev1.Pod, log *slog.Logger) VolumeWaitFunc {
	return func(missing []string) {
		now := metav1.NewTime(time.Now())
		for _, name := range missing {
			safeName := strings.ReplaceAll(name, "/", "-")
			eventName := fmt.Sprintf("%s.%s.%s", pod.Name, "FailedMount", safeName)
			existing, err := clientset.CoreV1().Events(pod.Namespace).Get(context.TODO(), eventName, metav1.GetOptions{})
			if err == nil {
				existing.Count++
				existing.LastTimestamp = now
				if _, err := clientset.CoreV1().Events(pod.Namespace).Update(context.TODO(), existing, metav1.UpdateOptions{}); err != nil {
					log.Warn("failed to update event", "pod", pod.Name, "error", err)
				}
				continue
			}

			event := &corev1.Event{
				ObjectMeta: metav1.ObjectMeta{
					Name:      eventName,
					Namespace: pod.Namespace,
				},
				InvolvedObject: corev1.ObjectReference{
					Kind:       "Pod",
					Name:       pod.Name,
					Namespace:  pod.Namespace,
					UID:        pod.UID,
					APIVersion: "v1",
				},
				Reason:         "FailedMount",
				Message:        fmt.Sprintf("MountVolume.SetUp failed for volume: %s not found", name),
				Type:           "Warning",
				Count:          1,
				FirstTimestamp: now,
				LastTimestamp:  now,
				Source: corev1.EventSource{
					Component: "kubelet",
				},
			}
			if _, err := clientset.CoreV1().Events(pod.Namespace).Create(context.TODO(), event, metav1.CreateOptions{}); err != nil {
				log.Warn("failed to emit event", "pod", pod.Name, "error", err)
			}
		}
	}
}

type saTokenResult struct {
	MountDir string
	Host     string
	Port     string
}

func materializeSAToken(ctx context.Context, clientset kubernetes.Interface, kubeconfigPath, volumesDir string, pod *corev1.Pod) (*saTokenResult, error) {
	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}

	expSeconds := int64(24 * 3600)
	tokenReq, err := clientset.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(ctx, saName, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &expSeconds,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating SA token: %w", err)
	}

	kubeConfig, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	var serverURL string
	var caData []byte
	for _, cluster := range kubeConfig.Clusters {
		serverURL = cluster.Server
		if cluster.CertificateAuthorityData != nil {
			caData = cluster.CertificateAuthorityData
		} else if cluster.CertificateAuthority != "" {
			caData, err = os.ReadFile(cluster.CertificateAuthority)
			if err != nil {
				return nil, fmt.Errorf("reading CA cert: %w", err)
			}
		}
		break
	}

	parsed, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("parsing server URL: %w", err)
	}

	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "443"
	}

	mountDir := filepath.Join(volumesDir, pod.Namespace, pod.Name, "sa-token")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating SA token dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(mountDir, "token"), []byte(tokenReq.Status.Token), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(mountDir, "ca.crt"), caData, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(mountDir, "namespace"), []byte(pod.Namespace), 0o644); err != nil {
		return nil, err
	}

	return &saTokenResult{MountDir: mountDir, Host: host, Port: port}, nil
}
