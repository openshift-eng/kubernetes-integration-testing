package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/controller/common"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := common.LoadConfig()
	if err != nil {
		log.Error("loading config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := controller.Run(ctx, log, cfg); err != nil {
		log.Error("controller failed", "error", err)
		os.Exit(1)
	}
}
