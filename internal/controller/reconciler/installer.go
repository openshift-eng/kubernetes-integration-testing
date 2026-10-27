package reconciler

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/dynutil"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

type ManifestInstaller struct {
	crdClient CRDClient
	dynClient dynamic.Interface
	disc      discovery.DiscoveryInterface
	log       *slog.Logger
}

const (
	kindCRD        = "CustomResourceDefinition"
	kindVAP        = "ValidatingAdmissionPolicy"
	kindVAPBinding = "ValidatingAdmissionPolicyBinding"
)

func NewManifestInstaller(crdClient CRDClient, dynClient dynamic.Interface, disc discovery.DiscoveryInterface, log *slog.Logger) *ManifestInstaller {
	return &ManifestInstaller{crdClient: crdClient, dynClient: dynClient, disc: disc, log: log}
}

func (i *ManifestInstaller) Install(ctx context.Context, fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("reading manifests: %w", err)
	}

	type manifest struct {
		name string
		raw  []byte
		kind string
	}

	var all []manifest
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("parsing kind from %s: %w", entry.Name(), err)
		}
		all = append(all, manifest{name: entry.Name(), raw: raw, kind: meta.Kind})
	}

	// CRDs must be established before CRs can reference them, and VAPs need their paramRef CRs to exist.
	// Within the same tier, sort by filename for determinism.
	sort.Slice(all, func(a, b int) bool {
		ta, tb := tierOrder(all[a].kind), tierOrder(all[b].kind)
		if ta != tb {
			return ta < tb
		}
		return all[a].name < all[b].name
	})

	var crdNames []string
	lastTier := -1

	for _, m := range all {
		tier := tierOrder(m.kind)

		if tier != lastTier && lastTier == 0 {
			if err := i.waitEstablished(ctx, crdNames); err != nil {
				return err
			}
		}
		lastTier = tier

		switch m.kind {
		case kindCRD:
			name, err := i.applyCRD(ctx, m.raw, m.name)
			if err != nil {
				return err
			}
			crdNames = append(crdNames, name)
		default:
			if err := i.applyResource(ctx, m.raw, m.name); err != nil {
				return err
			}
		}
	}

	if lastTier == 0 {
		if err := i.waitEstablished(ctx, crdNames); err != nil {
			return err
		}
	}

	return nil
}

func tierOrder(kind string) int {
	switch kind {
	case kindCRD:
		return 0
	case kindVAP, kindVAPBinding:
		return 2
	default:
		return 1
	}
}

func (i *ManifestInstaller) applyCRD(ctx context.Context, raw []byte, filename string) (string, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return "", fmt.Errorf("parsing %s: %w", filename, err)
	}

	existing, err := i.crdClient.Get(ctx, crd.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := i.crdClient.Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("creating CRD %s: %w", crd.Name, err)
		}
		i.log.Info("installed CRD", "name", crd.Name)
	} else if err != nil {
		return "", fmt.Errorf("getting CRD %s: %w", crd.Name, err)
	} else {
		crd.ResourceVersion = existing.ResourceVersion
		if _, err := i.crdClient.Update(ctx, &crd, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("updating CRD %s: %w", crd.Name, err)
		}
		i.log.Info("updated CRD", "name", crd.Name)
	}

	return crd.Name, nil
}

func (i *ManifestInstaller) waitEstablished(ctx context.Context, names []string) error {
	for _, name := range names {
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			got, err := i.crdClient.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			for _, c := range got.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return true, nil
				}
			}
			return false, nil
		}); err != nil {
			return fmt.Errorf("waiting for CRD %s: %w", name, err)
		}
	}
	return nil
}

func (i *ManifestInstaller) applyResource(ctx context.Context, raw []byte, filename string) error {
	var obj unstructured.Unstructured
	if err := yaml.Unmarshal(raw, &obj.Object); err != nil {
		return fmt.Errorf("parsing %s: %w", filename, err)
	}

	groupResources, err := restmapper.GetAPIGroupResources(i.disc)
	if err != nil {
		return fmt.Errorf("discovering API resources: %w", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	applied, err := dynutil.ApplyWithMapper(ctx, i.dynClient, mapper, &obj)
	if err != nil {
		return fmt.Errorf("applying %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	i.log.Info("installed manifest", "kind", applied.GetKind(), "name", applied.GetName())
	return nil
}
