package override

import (
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
)

// SelectMatch returns the single most specific override matching the pod, or
// nil if none match. Specificity is the number of active match constraints
// (namespace, each label entry, podName, nodeName). Ties are broken by
// lexicographically smallest metadata.name.
func SelectMatch(overrides []v1alpha1.WorkloadOverride, pod *corev1.Pod, podNamespace string) *v1alpha1.WorkloadOverride {
	var best *v1alpha1.WorkloadOverride
	bestScore := -1

	for i := range overrides {
		o := &overrides[i]
		if !MatchesPod(o.Spec.Match, pod, podNamespace) {
			continue
		}
		score := constraintCount(o.Spec.Match)
		if score > bestScore || (score == bestScore && o.Name < best.Name) {
			best = o
			bestScore = score
		}
	}
	return best
}

func MatchesPod(match v1alpha1.WorkloadOverrideMatch, pod *corev1.Pod, podNamespace string) bool {
	if match.Namespace != "" && match.Namespace != podNamespace {
		return false
	}
	if match.PodName != "" && match.PodName != pod.Name {
		return false
	}
	if match.NodeName != "" && match.NodeName != pod.Spec.NodeName {
		return false
	}
	for k, v := range match.Labels {
		if pod.Labels[k] != v {
			return false
		}
	}
	return true
}

func constraintCount(match v1alpha1.WorkloadOverrideMatch) int {
	n := len(match.Labels)
	if match.Namespace != "" {
		n++
	}
	if match.PodName != "" {
		n++
	}
	if match.NodeName != "" {
		n++
	}
	return n
}
