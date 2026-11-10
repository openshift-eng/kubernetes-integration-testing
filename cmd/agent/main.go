package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/afero"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
	gojaruntime "github.com/openshift-eng/kubernetes-integration-testing/internal/agent/goja"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := agent.LoadConfig()
	if err != nil {
		log.Error("loading config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx, log, afero.NewOsFs(), cfg, runtimeFactory); err != nil {
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
