package cmd

import (
	"fmt"

	"github.com/openshift-eng/kubernetes-integration-testing/pkg/version"
	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Version:          %s\n", version.Raw)
		fmt.Printf("Commit:           %s\n", version.Commit)
		fmt.Printf("Build Date:       %s\n", version.BuildDate)
		fmt.Printf("Default Arch:     %s\n", version.DefaultArch)
		fmt.Printf("Controller Image: %s\n", version.DefaultControllerImage)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
