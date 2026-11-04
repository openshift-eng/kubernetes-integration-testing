package cmd

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/afero"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemon"
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/store"
	"github.com/spf13/cobra"
)

var log *slog.Logger

var (
	appStore store.Store
	appPaths daemon.Paths
	appFs    afero.Fs
)

var rootCmd = &cobra.Command{
	Use:   "kit",
	Short: "Kubernetes Integration Testing tool",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		root := filepath.Join(home, ".local", "share", "openshift-kit")
		appPaths = daemon.NewPaths(root)
		appFs = afero.NewOsFs()

		s, err := store.New()
		if err != nil {
			return err
		}
		appStore = s
		return nil
	},
}

func init() {
	log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
