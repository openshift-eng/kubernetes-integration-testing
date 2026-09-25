package cmd

import (
	"github.com/openshift-eng/kubernetes-integration-testing/internal/store"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	Run: func(cmd *cobra.Command, args []string) {
		pid, running := store.IsRunning(appStore.PidPath(), appStore.SocketPath())
		if running {
			log.Info("running", "pid", pid)
		} else {
			log.Info("not running")
		}
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
