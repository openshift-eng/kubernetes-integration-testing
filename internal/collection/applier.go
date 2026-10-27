package collection

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/dynutil"
)

// TODO: once CEL-based status patches live in the ResourceCollection spec,
// StatusPatcher and the Applier itself can be removed — the reconciler will
// apply objects directly and evaluate CEL expressions for status patches.
type StatusPatcher interface {
	Matches(obj *unstructured.Unstructured) bool
	Patch(ctx context.Context, client dynamic.Interface, applied *unstructured.Unstructured) error
}

type Applier struct {
	client   dynamic.Interface
	mapper   meta.RESTMapper
	patchers []StatusPatcher
	log      *slog.Logger
}

func NewApplier(client dynamic.Interface, mapper meta.RESTMapper, log *slog.Logger) *Applier {
	return &Applier{client: client, mapper: mapper, log: log}
}

func (a *Applier) WithStatusPatcher(p StatusPatcher) *Applier {
	a.patchers = append(a.patchers, p)
	return a
}

func (a *Applier) ApplyRendered(ctx context.Context, result *RenderResult) error {
	for _, obj := range result.Objects {
		applied, err := dynutil.ApplyWithMapper(ctx, a.client, a.mapper, obj)
		if err != nil {
			return fmt.Errorf("applying %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		a.log.Info("applied collection resource", "kind", applied.GetKind(), "name", applied.GetName())

		for _, p := range a.patchers {
			if p.Matches(applied) {
				if err := p.Patch(ctx, a.client, applied); err != nil {
					return fmt.Errorf("patching status for %s %s: %w", applied.GetKind(), applied.GetName(), err)
				}
			}
		}
	}
	return nil
}
