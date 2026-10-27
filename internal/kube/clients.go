package kube

import (
	"fmt"

	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Clients struct {
	REST       *rest.Config
	Client     client.Client
	Dynamic    dynamic.Interface
	Discovery  discovery.DiscoveryInterface
	Extensions apiextclient.Interface
}

func NewClients(kubeConfig *KubeConfig) (*Clients, error) {
	cfg, err := kubeConfig.RestConfig()
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}
	cfg.QPS = 50
	cfg.Burst = 100

	extClient, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating apiextensions client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating discovery client: %w", err)
	}

	cl, err := client.New(cfg, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating controller-runtime client: %w", err)
	}

	return &Clients{
		REST:       cfg,
		Client:     cl,
		Dynamic:    dynClient,
		Discovery:  disc,
		Extensions: extClient,
	}, nil
}
