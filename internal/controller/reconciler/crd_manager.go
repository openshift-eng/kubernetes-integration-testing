package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/release"

	"golang.org/x/sync/errgroup"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
)

type CRDClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*apiextensionsv1.CustomResourceDefinition, error)
	Create(ctx context.Context, crd *apiextensionsv1.CustomResourceDefinition, opts metav1.CreateOptions) (*apiextensionsv1.CustomResourceDefinition, error)
	Update(ctx context.Context, crd *apiextensionsv1.CustomResourceDefinition, opts metav1.UpdateOptions) (*apiextensionsv1.CustomResourceDefinition, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

type CRDManager struct {
	client CRDClient
	log    *slog.Logger
}

func NewCRDManager(client CRDClient, log *slog.Logger) *CRDManager {
	return &CRDManager{client: client, log: log}
}

func (m *CRDManager) Sync(ctx context.Context, crds []release.Manifest, featureSet string) error {
	var toApply, toDelete []release.Manifest
	for _, crd := range crds {
		if !release.MatchesProfile(crd.Obj.GetAnnotations(), featureSet) {
			continue
		}
		if release.IsDelete(crd) {
			toDelete = append(toDelete, crd)
		} else {
			toApply = append(toApply, crd)
		}
	}

	m.log.Info("syncing CRDs", "apply", len(toApply), "delete", len(toDelete))

	if err := m.apply(ctx, toApply); err != nil {
		return err
	}

	return m.delete(ctx, toDelete)
}

func (m *CRDManager) apply(ctx context.Context, crds []release.Manifest) error {
	if len(crds) == 0 {
		return nil
	}

	var mu sync.Mutex
	var applied []string

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(10)
	for _, manifest := range crds {
		g.Go(func() error {
			var required apiextensionsv1.CustomResourceDefinition
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(manifest.Obj.Object, &required); err != nil {
				return fmt.Errorf("converting CRD %s: %w", manifest.Obj.GetName(), err)
			}

			existing, err := m.client.Get(gCtx, required.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				if _, err := m.client.Create(gCtx, &required, metav1.CreateOptions{}); err != nil {
					return fmt.Errorf("creating CRD %s: %w", required.Name, err)
				}
				mu.Lock()
				applied = append(applied, required.Name)
				mu.Unlock()
				return nil
			}
			if err != nil {
				return fmt.Errorf("getting CRD %s: %w", required.Name, err)
			}

			if !equality.Semantic.DeepEqual(existing.Spec, required.Spec) {
				required.ResourceVersion = existing.ResourceVersion
				if _, err := m.client.Update(gCtx, &required, metav1.UpdateOptions{}); err != nil {
					return fmt.Errorf("updating CRD %s: %w", required.Name, err)
				}
			}
			mu.Lock()
			applied = append(applied, required.Name)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	return m.waitEstablished(ctx, applied)
}

func (m *CRDManager) delete(ctx context.Context, crds []release.Manifest) error {
	if len(crds) == 0 {
		return nil
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(10)
	for _, manifest := range crds {
		name := manifest.Obj.GetName()
		g.Go(func() error {
			if err := m.client.Delete(gCtx, name, metav1.DeleteOptions{}); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return fmt.Errorf("deleting CRD %s: %w", name, err)
			}
			m.log.Info("deleted CRD", "name", name)
			return nil
		})
	}
	return g.Wait()
}

func (m *CRDManager) waitEstablished(ctx context.Context, names []string) error {
	wg, wCtx := errgroup.WithContext(ctx)
	for _, name := range names {
		wg.Go(func() error {
			return wait.PollUntilContextTimeout(wCtx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
				crd, err := m.client.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				for _, c := range crd.Status.Conditions {
					if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
						return true, nil
					}
				}
				return false, nil
			})
		})
	}
	return wg.Wait()
}
