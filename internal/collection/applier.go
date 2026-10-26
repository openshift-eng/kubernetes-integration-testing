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

type StatusPatcher interface {
	Matches(obj *unstructured.Unstructured) bool
	Patch(ctx context.Context, client dynamic.Interface, applied *unstructured.Unstructured) error
}

type Applier struct {
	coll     *Collection
	client   dynamic.Interface
	mapper   meta.RESTMapper
	patchers []StatusPatcher
	log      *slog.Logger
}

func NewApplier(coll *Collection, client dynamic.Interface, mapper meta.RESTMapper, log *slog.Logger) *Applier {
	return &Applier{coll: coll, client: client, mapper: mapper, log: log}
}

func (a *Applier) WithStatusPatcher(p StatusPatcher) *Applier {
	a.patchers = append(a.patchers, p)
	return a
}

func (a *Applier) Apply(ctx context.Context, renderCtx RenderContext) error {
	objects, err := a.coll.Render(renderCtx)
	if err != nil {
		return fmt.Errorf("rendering collection %q: %w", a.coll.Name(), err)
	}

	for _, obj := range objects {
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
