package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/workload/override"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const PausePath = "/mutate-pods"

type PodMutator struct {
	Client  client.Client
	Decoder admission.Decoder
	Log     *slog.Logger
}

func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var pod corev1.Pod
	if err := m.Decoder.Decode(req, &pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
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
