package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	agentctl "github.com/openshift-eng/kubernetes-integration-testing/internal/controller/agent"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/workload/override"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const PausePath = "/mutate-pods"

type PodMutator struct {
	Client       client.Client
	Materializer *agentctl.Materializer
	Decoder      admission.Decoder
	Log          *slog.Logger
}

func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var pod corev1.Pod
	if err := m.Decoder.Decode(req, &pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	isDryRun := req.DryRun != nil && *req.DryRun
	if !isDryRun && m.Materializer != nil {
		if err := m.ensureAgentOverrides(ctx, &pod, req.Namespace); err != nil {
			m.Log.Error("materializing agent overrides", "namespace", req.Namespace, "error", err)
			return admission.Errored(http.StatusInternalServerError,
				fmt.Errorf("materializing agent overrides: %w", err))
		}
	}

	var overrides v1alpha1.WorkloadOverrideList
	if err := m.Client.List(ctx, &overrides); err != nil {
		m.Log.Error("listing WorkloadOverrides", "error", err)
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("listing WorkloadOverrides: %w", err))
	}

	mutated := pod.DeepCopy()
	result := override.Apply(mutated, overrides.Items, req.Namespace)
	switch result.Outcome {
	case override.NoMatch:
		return admission.Allowed("no matching override")
	case override.Invalid:
		m.Log.Warn("override matched but could not be applied",
			"pod", pod.Name, "namespace", req.Namespace, "override", result.Override.Name, "reason", result.Reason)
		return admission.Allowed("override invalid for pod").
			WithWarnings(fmt.Sprintf("WorkloadOverride %q: %s", result.Override.Name, result.Reason))
	}

	m.Log.Info("applied override", "pod", pod.Name, "namespace", req.Namespace,
		"override", result.Override.Name)

	modified, err := json.Marshal(mutated)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("marshaling mutated pod: %w", err))
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, modified)
}

// ensureAgentOverrides materializes any Agent whose target matches this pod
// but whose generated WorkloadOverride is missing or stale.
//
// TODO: IsWOFresh is a reconciliation watermark (owner + generation + digest).
// It does not detect manual spec edits that preserve the digest env var.
// Full drift repair would require comparing the complete WO spec.
//
// TODO: concurrent admissions or admission+reconciler can race on WO updates,
// causing a conflict that rejects the pod. Add retry on IsConflict or switch
// ensureWorkloadOverride to SSA.
func (m *PodMutator) ensureAgentOverrides(ctx context.Context, pod *corev1.Pod, namespace string) error {
	var agents v1alpha1.AgentList
	if err := m.Client.List(ctx, &agents); err != nil {
		return fmt.Errorf("listing Agents: %w", err)
	}

	var overrides v1alpha1.WorkloadOverrideList
	if err := m.Client.List(ctx, &overrides); err != nil {
		return fmt.Errorf("listing WorkloadOverrides: %w", err)
	}

	existing := make(map[string]*v1alpha1.WorkloadOverride, len(overrides.Items))
	for i := range overrides.Items {
		existing[overrides.Items[i].Name] = &overrides.Items[i]
	}

	for i := range agents.Items {
		ag := &agents.Items[i]
		if !agentMatchesPod(ag, pod, namespace) {
			continue
		}
		woName := agentctl.GeneratedName(ag.Name)
		if wo, ok := existing[woName]; ok && agentctl.IsWOFresh(ag, wo) {
			continue
		}
		if _, err := m.Materializer.Materialize(ctx, ag); err != nil {
			return fmt.Errorf("materializing agent %q: %w", ag.Name, err)
		}
		m.Log.Info("webhook materialized agent", "agent", ag.Name, "namespace", namespace)
	}
	return nil
}

func agentMatchesPod(ag *v1alpha1.Agent, pod *corev1.Pod, namespace string) bool {
	return override.MatchesPod(v1alpha1.WorkloadOverrideMatch{
		Namespace: ag.Spec.Target.Namespace,
		Labels:    ag.Spec.Target.Labels,
		PodName:   ag.Spec.Target.PodName,
		NodeName:  ag.Spec.Target.NodeName,
	}, pod, namespace)
}
