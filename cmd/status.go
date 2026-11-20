package cmd

import (
	"github.com/spf13/cobra"

	"github.com/openshift-eng/kubernetes-integration-testing/pkg/client"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, err := client.Status()
		if err != nil {
			return err
		}
		if st.Running {
			if st.Pid > 0 {
				log.Info("running", "pid", st.Pid)
			} else {
				log.Info("running")
			}
		} else {
			log.Info("not running")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
