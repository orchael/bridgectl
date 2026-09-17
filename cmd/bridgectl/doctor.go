package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Check local bridgectl health", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := readEnrollmentMetadata()
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintln(out, "Bridge")
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintln(out, "  server        - not configured")
			_, _ = fmt.Fprintln(out, "  enrollment    - not logged in")
			return nil
		}
		if err != nil {
			_, _ = fmt.Fprintf(out, "  enrollment    ! unreadable (%v)\n", err)
			return nil
		}
		_, _ = fmt.Fprintf(out, "  server        ✓ %s\n", e.BridgeURL)
		_, _ = fmt.Fprintf(out, "  enrollment    ✓ logged in\n")
		_, _ = fmt.Fprintf(out, "  organization  ✓ %s\n", display(e.OrganizationName, e.OrganizationID))
		_, _ = fmt.Fprintf(out, "  installation  ✓ %s\n", display(e.InstallationName, e.InstallationID))
		if s, secretErr := readBridgeSecret(); secretErr != nil || s == nil || s.CollectorCredential == "" {
			_, _ = fmt.Fprintln(out, "  telemetry     ! credential missing")
		} else {
			_, _ = fmt.Fprintln(out, "  telemetry     ✓ configured")
		}
		return nil
	}}
}
