package cmd

import (
	"fmt"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemon"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := daemon.Stop(appFs, appPaths.Pid(), appPaths.Socket()); err != nil {
			return fmt.Errorf("stopping: %w", err)
		}
		log.Info("stopped")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
