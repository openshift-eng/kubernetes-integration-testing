package collection

import (
	"context"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var configGV = schema.GroupVersion{Group: "config.openshift.io", Version: "v1"}

type InfrastructureStatusPatcher struct {
	apiServerURL string
	platform     string
	log          *slog.Logger
}

func NewInfrastructureStatusPatcher(apiServerURL, platform string, log *slog.Logger) *InfrastructureStatusPatcher {
	return &InfrastructureStatusPatcher{apiServerURL: apiServerURL, platform: platform, log: log}
}

func (p *InfrastructureStatusPatcher) Matches(obj *unstructured.Unstructured) bool {
	return obj.GroupVersionKind() == configGV.WithKind("Infrastructure")
}

func (p *InfrastructureStatusPatcher) Patch(ctx context.Context, client dynamic.Interface, applied *unstructured.Unstructured) error {
	res := configGV.WithResource("infrastructures")
	applied.Object["status"] = map[string]interface{}{
		"platform": p.platform,
		"platformStatus": map[string]interface{}{
			"type": p.platform,
		},
		"apiServerInternalURI": p.apiServerURL,
	}
	_, err := client.Resource(res).UpdateStatus(ctx, applied, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating Infrastructure status: %w", err)
	}
	p.log.Info("patched Infrastructure status", "apiServerInternalURL", p.apiServerURL)
	return nil
}

type ClusterVersionStatusPatcher struct {
	image   string
	version string
	log     *slog.Logger
}

func NewClusterVersionStatusPatcher(image, version string, log *slog.Logger) *ClusterVersionStatusPatcher {
	return &ClusterVersionStatusPatcher{image: image, version: version, log: log}
}

func (p *ClusterVersionStatusPatcher) Matches(obj *unstructured.Unstructured) bool {
	return obj.GroupVersionKind() == configGV.WithKind("ClusterVersion")
}

func (p *ClusterVersionStatusPatcher) Patch(ctx context.Context, client dynamic.Interface, applied *unstructured.Unstructured) error {
	res := configGV.WithResource("clusterversions")
	applied.Object["status"] = map[string]interface{}{
		"desired": map[string]interface{}{
			"image":   p.image,
			"version": p.version,
		},
		"observedGeneration": 0,
		"versionHash":        "",
		"availableUpdates":   []interface{}{},
	}
	_, err := client.Resource(res).UpdateStatus(ctx, applied, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating ClusterVersion status: %w", err)
	}
	p.log.Info("patched ClusterVersion status", "version", p.version)
	return nil
}

type FeatureGateStatusPatcher struct {
	featureSet string
	version    string
	manifests  []*unstructured.Unstructured
	log        *slog.Logger
}

func NewFeatureGateStatusPatcher(featureSet, version string, manifests []*unstructured.Unstructured, log *slog.Logger) *FeatureGateStatusPatcher {
	return &FeatureGateStatusPatcher{featureSet: featureSet, version: version, manifests: manifests, log: log}
}

func (p *FeatureGateStatusPatcher) Matches(obj *unstructured.Unstructured) bool {
	return obj.GroupVersionKind() == configGV.WithKind("FeatureGate")
}

func (p *FeatureGateStatusPatcher) Patch(ctx context.Context, client dynamic.Interface, applied *unstructured.Unstructured) error {
	fgStatus := findFeatureGateStatus(p.manifests, p.featureSet)
	if fgStatus == nil {
		return nil
	}

	if p.version != "" {
		if gates, ok := fgStatus["featureGates"].([]interface{}); ok {
			for _, g := range gates {
				if entry, ok := g.(map[string]interface{}); ok {
					entry["version"] = p.version
				}
			}
		}
	}

	res := configGV.WithResource("featuregates")
	applied.Object["status"] = fgStatus
	_, err := client.Resource(res).UpdateStatus(ctx, applied, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating FeatureGate status: %w", err)
	}
	p.log.Info("patched FeatureGate status from release manifest", "version", p.version)
	return nil
}

func findFeatureGateStatus(manifests []*unstructured.Unstructured, featureSet string) map[string]interface{} {
	if featureSet == "" {
		featureSet = "Default"
	}
	for _, obj := range manifests {
		if obj.GetKind() != "FeatureGate" || obj.GetName() != "cluster" {
			continue
		}
		annotations := obj.GetAnnotations()
		fsValue, ok := annotations["release.openshift.io/feature-set"]
		if !ok || fsValue != featureSet {
			continue
		}
		status, ok := obj.Object["status"].(map[string]interface{})
		if ok {
			return status
		}
	}
	return nil
}
