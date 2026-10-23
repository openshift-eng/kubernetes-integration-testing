package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/openshift-eng/kubernetes-integration-testing/api/crds"
	v1alpha1 "github.com/openshift-eng/kubernetes-integration-testing/api/k8s/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

var scheme = runtime.NewScheme()

func init() {
	_ = v1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
}

func installCRDs(ctx context.Context, log *slog.Logger, cfg *rest.Config) error {
	extClient, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return err
	}

	entries, err := crds.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("reading embedded CRDs: %w", err)
	}

	crdClient := extClient.ApiextensionsV1().CustomResourceDefinitions()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := crds.FS.ReadFile(entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}

		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return fmt.Errorf("parsing %s: %w", entry.Name(), err)
		}

		existing, err := crdClient.Get(ctx, crd.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if _, err := crdClient.Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("creating CRD %s: %w", crd.Name, err)
			}
			log.Info("installed CRD", "name", crd.Name)
		} else if err != nil {
			return fmt.Errorf("getting CRD %s: %w", crd.Name, err)
		} else {
			crd.ResourceVersion = existing.ResourceVersion
			if _, err := crdClient.Update(ctx, &crd, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("updating CRD %s: %w", crd.Name, err)
			}
			log.Info("updated CRD", "name", crd.Name)
		}

		if err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			got, err := crdClient.Get(ctx, crd.Name, metav1.GetOptions{})
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
			return fmt.Errorf("waiting for CRD %s: %w", crd.Name, err)
		}
	}

	return nil
}
