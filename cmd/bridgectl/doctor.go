package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Check local bridgectl health", RunE: func(cmd *cobra.Command, _ []string) error {
		e, s, err := readEnrollment()
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Bridge")
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(out, "  server        - not configured")
			fmt.Fprintln(out, "  enrollment    - not logged in")
			return nil
		}
		if err != nil {
			fmt.Fprintf(out, "  enrollment    ! unreadable (%v)\n", err)
			return nil
		}
		fmt.Fprintf(out, "  server        ✓ %s\n", e.BridgeURL)
		fmt.Fprintf(out, "  enrollment    ✓ logged in\n")
		fmt.Fprintf(out, "  organization  ✓ %s\n", display(e.OrganizationName, e.OrganizationID))
		fmt.Fprintf(out, "  installation  ✓ %s\n", display(e.InstallationName, e.InstallationID))
		if s == nil || s.CollectorCredential == "" {
			fmt.Fprintln(out, "  telemetry     ! credential missing")
		} else {
			fmt.Fprintln(out, "  telemetry     ✓ configured")
		}
		return nil
	}}
}
