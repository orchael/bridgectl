package main

import (
	"github.com/orchael/bridgectl/internal/claudehooks"
	"github.com/spf13/cobra"
)

func newSessionHookCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "report-claude-hook",
		Short: "Forward structured Claude hook metadata to a local session observer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return claudehooks.Report(cmd.Context(), configPath, cmd.InOrStdin())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "private observer configuration (automatically supplied by the session provider)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
