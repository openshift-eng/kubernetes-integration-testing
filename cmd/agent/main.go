package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/afero"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/agent"
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

	if err := agent.Run(ctx, log, afero.NewOsFs(), cfg); err != nil {
		log.Error("agent failed", "error", err)
		os.Exit(1)
	}
}
