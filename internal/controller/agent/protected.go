package agent

import (
	"context"
	"fmt"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/bundle"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProtectedBundleState returns a ProtectedState callback that snapshots
// live Agents (for mapping purge) and all digests referenced by
// WorkloadOverride env vars and Agent status (for blob protection).
func ProtectedBundleState(c client.Client) bundle.ProtectedState {
	return func(ctx context.Context) (bundle.ClusterSnapshot, error) {
		var snap bundle.ClusterSnapshot
		snap.ActiveAgents = make(map[string]struct{})
		snap.ProtectedDigests = make(map[string]struct{})

		var agents v1alpha1.AgentList
		if err := c.List(ctx, &agents); err != nil {
			return snap, fmt.Errorf("listing Agents: %w", err)
		}
		for i := range agents.Items {
			ag := &agents.Items[i]
			snap.ActiveAgents[ag.Name] = struct{}{}
			if d := ag.Status.BundleDigest; bundle.IsValidDigest(d) {
				snap.ProtectedDigests[d] = struct{}{}
			}
		}

		var wos v1alpha1.WorkloadOverrideList
		if err := c.List(ctx, &wos); err != nil {
			return snap, fmt.Errorf("listing WorkloadOverrides: %w", err)
		}
		for i := range wos.Items {
			wo := &wos.Items[i]
			if wo.Spec.ReplacePod != nil {
				for _, e := range wo.Spec.ReplacePod.Env {
					if e.Name == "KIT_AGENT_BUNDLE_SHA256" && bundle.IsValidDigest(e.Value) {
						snap.ProtectedDigests[e.Value] = struct{}{}
					}
				}
			}
			for j := range wo.Spec.Containers {
				for _, e := range wo.Spec.Containers[j].Env {
					if e.Name == "KIT_AGENT_BUNDLE_SHA256" && bundle.IsValidDigest(e.Value) {
						snap.ProtectedDigests[e.Value] = struct{}{}
					}
				}
			}
		}

		return snap, nil
	}
}
