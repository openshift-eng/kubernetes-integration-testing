package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/afero"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
	gojaruntime "github.com/openshift-eng/kubernetes-integration-testing/internal/agent/goja"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent/kube"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := agent.LoadConfig()
	if err != nil {
		log.Error("loading config", "error", err)
		os.Exit(1)
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Error("loading in-cluster config", "error", err)
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		log.Error("creating dynamic client", "error", err)
		os.Exit(1)
	}

	disco, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		log.Error("creating discovery client", "error", err)
		os.Exit(1)
	}

	groupResources, err := restmapper.GetAPIGroupResources(disco)
	if err != nil {
		log.Error("discovering API groups", "error", err)
		os.Exit(1)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	kubeClient := kube.NewClient(dynClient, mapper, kube.NewPolicy())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx, log, afero.NewOsFs(), cfg, runtimeFactory, kubeClient); err != nil {
		log.Error("agent failed", "error", err)
		os.Exit(1)
	}
}

func runtimeFactory(name string) (agent.Runtime, error) {
	switch name {
	case "js":
		return gojaruntime.New(), nil
	default:
		return nil, fmt.Errorf("unsupported runtime: %q", name)
	}
}
