package cmd

import (
	"fmt"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/store"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := store.Stop(appStore.PidPath(), appStore.SocketPath()); err != nil {
			return fmt.Errorf("stopping: %w", err)
		}
		log.Info("stopped")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
