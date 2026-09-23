package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchael/bridgectl/internal/bridgecontrol"
	"github.com/orchael/bridgectl/internal/localserver"
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
		secret, secretErr := readBridgeSecret()
		if secretErr != nil || secret == nil || secret.CollectorCredential == "" {
			_, _ = fmt.Fprintln(out, "  telemetry     ! credential missing")
		} else {
			_, _ = fmt.Fprintln(out, "  telemetry     ✓ configured")
		}
		controlCredential, _ := readControlCredential() // a read error just means "not usable" here
		_, _ = fmt.Fprintln(out, "  control       "+controlDoctorLine(e, controlCredential))
		return nil
	}}
}

// controlDoctorLine reports the control-plane connection state for
// `bridgectl doctor`. It never opens its own WebSocket connection: doing so
// with the shared installation credential would trigger Bridge's
// generation-fencing and evict the daemon's real, already-live connection
// (see docs on control/connection.go's "superseded" behavior). Instead it
// reads the small status file the daemon's control client persists on every
// state change — and on every change only, not periodically, so a "connected"
// entry can be arbitrarily old for a perfectly healthy, long-lived
// connection. To still catch an unclean daemon death (killed or powered off
// without a chance to write a final status), the control client itself
// refreshes the "connected" entry's timestamp on every heartbeat; an entry
// older than bridgecontrol.StatusStaleAfter is therefore stale enough that
// no live client can currently be behind it, and is reported as
// disconnected rather than trusted at face value.
func controlDoctorLine(e *bridgeEnrollment, controlCredential string) string {
	if !controlProvisioned(e, controlCredential) {
		return "- not provisioned — run bridgectl bridge login --force"
	}
	statusPath := filepath.Join(localserver.StateDir(), "bridge-control-status.json")
	st, err := bridgecontrol.ReadStatus(statusPath)
	if err != nil {
		return "! disconnected"
	}
	if st.State == bridgecontrol.StateConnected && time.Since(st.UpdatedAt) > bridgecontrol.StatusStaleAfter {
		return "! disconnected"
	}
	switch st.State {
	case bridgecontrol.StateConnected:
		return "✓ connected"
	case bridgecontrol.StateAuthRejected:
		return "! authentication rejected"
	case bridgecontrol.StateUnavailable:
		return "! Bridge unavailable"
	case bridgecontrol.StateConnecting:
		return "! connecting"
	default:
		return "! disconnected"
	}
}
