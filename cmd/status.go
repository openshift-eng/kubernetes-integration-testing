package cmd

import (
	"github.com/openshift-eng/kubernetes-integration-testing/internal/host/daemon"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	Run: func(cmd *cobra.Command, args []string) {
		pid, running := daemon.IsRunning(appFs, appPaths.Pid(), appPaths.Socket())
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
