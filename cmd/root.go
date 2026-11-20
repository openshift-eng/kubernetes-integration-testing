package cmd

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var log *slog.Logger

var rootCmd = &cobra.Command{
	Use:   "kit",
	Short: "Kubernetes Integration Testing tool",
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
