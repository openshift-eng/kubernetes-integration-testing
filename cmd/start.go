package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/afero"
	"github.com/spf13/cobra"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/cluster"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemon"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemonstate"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/provider/kind"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/provider/kwok"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/store"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/podman"
	"github.com/openshift-eng/kubernetes-integration-testing/pkg/client"
)

var foreground bool

// startGrace is how long `kit start` watches the spawned daemon for an
// immediate exit before reporting success.
const startGrace = 2 * time.Second

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		// The instance lock is the authority on whether a daemon is
		// running; the daemon enforces it again at startup.
		if st, err := client.Status(); err != nil {
			return err
		} else if st.Running {
			return fmt.Errorf("already running (pid %d)", st.Pid)
		}

		if foreground {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			paths, err := daemonstate.DefaultPaths()
			if err != nil {
				return err
			}

			// The store is daemon assembly state: it is initialized here,
			// in the only command that runs the server, and nowhere else.
			appStore, err := store.NewWithRoot(afero.NewOsFs(), paths.Root)
			if err != nil {
				return fmt.Errorf("initializing store: %w", err)
			}

			podmanClient := podman.NewLocalClient()
			factories := map[string]daemon.ProviderFactory{
				"kwok": func(ctx context.Context, log *slog.Logger) cluster.Provider {
					return kwok.NewProvider(ctx, appStore, log.With("module", "kwok"))
				},
				"kind": func(ctx context.Context, log *slog.Logger) cluster.Provider {
					return kind.NewProvider(ctx, appStore, podmanClient, log.With("module", "kind"))
				},
			}
			return daemon.Start(ctx, paths, appStore, podmanClient, factories)
		}

		exe, err := os.Executable()
		if err != nil {
			return err
		}
		pid, err := client.StartDaemonBackground(exe, startGrace)
		if err != nil {
			return err
		}
		log.Info("started", "pid", pid)
		return nil
	},
}

func init() {
	startCmd.Flags().BoolVar(&foreground, "foreground", false, "run in foreground (for systemd/launchd)")
	rootCmd.AddCommand(startCmd)
}
