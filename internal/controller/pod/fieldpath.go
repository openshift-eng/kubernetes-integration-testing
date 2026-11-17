package pod

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ResolveFieldRef resolves a downward API fieldPath against a pod.
// Covers the basic pod-level fields needed by Kit agents; the upstream
// implementation lives in k8s.io/kubernetes/pkg/fieldpath which
// is too heavy to import directly.
func ResolveFieldRef(pod *corev1.Pod, fieldPath string) (string, error) {
	switch fieldPath {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return string(pod.UID), nil
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	default:
		return "", fmt.Errorf("unsupported fieldPath %q", fieldPath)
	}
}
