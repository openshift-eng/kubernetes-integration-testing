package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/openshift-eng/kubernetes-integration-testing/pkg/client"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := client.ShutdownDaemon(context.Background()); err != nil {
			return fmt.Errorf("stopping: %w", err)
		}
		log.Info("stopped")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
