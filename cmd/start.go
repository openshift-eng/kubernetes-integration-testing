package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemon"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/provider/kind"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/provider/kwok"
	"github.com/spf13/cobra"
)

var foreground bool

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		if pid, running := daemon.IsRunning(appFs, appPaths.Pid(), appPaths.Socket()); running {
			return fmt.Errorf("already running (pid %d)", pid)
		}

		if foreground {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			factories := map[string]daemon.ProviderFactory{
				"kwok": func(ctx context.Context, log *slog.Logger) cluster.Provider {
					return kwok.NewProvider(ctx, appStore, log.With("module", "kwok"))
				},
				"kind": func(ctx context.Context, log *slog.Logger) cluster.Provider {
					return kind.NewProvider(ctx, appStore, log.With("module", "kind"))
				},
			}
			return daemon.Start(ctx, appPaths, appStore.ImageCacheDir(), factories)
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
