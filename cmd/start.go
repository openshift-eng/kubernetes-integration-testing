package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/daemon"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kind"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/kwok"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/store"
	"github.com/spf13/cobra"
)

var foreground bool

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		if pid, running := store.IsRunning(appStore.PidPath(), appStore.SocketPath()); running {
			return fmt.Errorf("already running (pid %d)", pid)
		}

		if foreground {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			factories := map[string]daemon.ProviderFactory{
				"kwok": func(log *slog.Logger) cluster.Provider {
					return kwok.NewProvider(appStore, log.With("module", "kwok"))
				},
				"kind": func(log *slog.Logger) cluster.Provider {
					return kind.NewProvider(appStore, log.With("module", "kind"))
				},
			}
			return daemon.Run(ctx, appStore, factories, true)
		}
		return runBackground()
	},
}

func init() {
	startCmd.Flags().BoolVar(&foreground, "foreground", false, "run in foreground (for systemd/launchd)")
	rootCmd.AddCommand(startCmd)
}

func runBackground() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	child := exec.Command(exe, "start", "--foreground")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := child.Start(); err != nil {
		return fmt.Errorf("starting background process: %w", err)
	}

	log.Info("started", "pid", child.Process.Pid)
	return nil
}
