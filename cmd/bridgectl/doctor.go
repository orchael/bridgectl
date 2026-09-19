package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// bridgeReachable does a best-effort, short-timeout connectivity check. It
// never fails doctor: an inconclusive result is reported as unreachable
// rather than blocking or erroring out the rest of the report.
func bridgeReachable(url string) bool {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

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
		if bridgeReachable(e.BridgeURL) {
			_, _ = fmt.Fprintln(out, "  network       ✓ reachable")
		} else {
			_, _ = fmt.Fprintln(out, "  network       ! unreachable")
		}
		_, _ = fmt.Fprintf(out, "  enrollment    ✓ logged in\n")
		_, _ = fmt.Fprintf(out, "  organization  ✓ %s\n", display(e.OrganizationName, e.OrganizationID))
		if e.OrganizationURL != "" {
			_, _ = fmt.Fprintf(out, "  org url       ✓ %s\n", e.OrganizationURL)
		}
		_, _ = fmt.Fprintf(out, "  installation  ✓ %s\n", display(e.InstallationName, e.InstallationID))
		if s, secretErr := readBridgeSecret(); secretErr != nil || s == nil || s.CollectorCredential == "" {
			_, _ = fmt.Fprintln(out, "  telemetry     ! credential missing")
		} else {
			_, _ = fmt.Fprintln(out, "  telemetry     ✓ configured")
		}
		return nil
	}}
}
