package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchael/bridgectl/internal/bridgecontrol"
	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/localserver"
)

// doctorTimeout bounds every network- or subprocess-backed doctor check so a
// slow or hung dependency can never stall the command; see bridgeReachable
// for the same pattern applied to the Bridge connectivity check.
const doctorTimeout = 3 * time.Second

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
		out := cmd.OutOrStdout()
		printBridgeSection(out)
		_, _ = fmt.Fprintln(out)
		printVersionsSection(cmd.Context(), out)
		return nil
	}}
}

// printBridgeSection prints the connectivity/enrollment report. It never
// returns an error: an unreadable or missing enrollment file is itself a
// finding, reported inline, not a reason to abort the rest of doctor.
func printBridgeSection(out io.Writer) {
	e, err := readEnrollmentMetadata()
	_, _ = fmt.Fprintln(out, "Bridge")
	if errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintln(out, "  server        - not configured")
		_, _ = fmt.Fprintln(out, "  enrollment    - not logged in")
		return
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "  enrollment    ! unreadable (%v)\n", err)
		return
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
}

// printVersionsSection reports the bridgectl, Node, and running-daemon
// versions doctor can check offline and without any installed provider.
// Provider CLI version drift and macOS launch-agent staleness are tracked
// separately (see issue #265) and intentionally left out of this section.
func printVersionsSection(ctx context.Context, out io.Writer) {
	_, _ = fmt.Fprintln(out, "Versions")
	_, _ = fmt.Fprintf(out, "  bridgectl     ✓ %s\n", version)
	_, _ = fmt.Fprintln(out, nodeVersionLine(ctx, doctorProviderRoot()))
	_, _ = fmt.Fprintln(out, serverVersionLine(ctx))
}

// doctorProviderRoot resolves the directory .nvmrc is read from: the
// configured runtime.provider_root when a config file is found, otherwise
// the daemon's working directory convention of "." (CWD-relative), matching
// the compatibility rule documented on config.RuntimeConfig.
//
// It reuses defaultServerConfigPath, the same discovery `server start` uses
// (stateDir/bridge.yaml, then $XDG_CONFIG_HOME/bridgectl/config.yaml, then
// the OS user-config dir) so doctor checks the config the daemon would
// actually load by default, not just the stateDir candidate. An explicit
// `--config` path passed to a running daemon is not discoverable here: the
// daemon does not currently persist which config path it loaded, so a
// doctor run against such a daemon can still report Node status against
// the wrong .nvmrc. Persisting the active config path is a larger change
// left for a follow-up; it is not one of the core items in issue #265.
func doctorProviderRoot() string {
	configPath := defaultServerConfigPath(localserver.StateDir())
	if configPath == "" {
		return "."
	}
	if fileCfg, err := config.Load(configPath); err == nil && fileCfg.Runtime.ProviderRoot != "" {
		return fileCfg.Runtime.ProviderRoot
	}
	return "."
}

// nodeVersionLine reports Node's version against the .nvmrc requirement.
// It gives config.ValidateNodeRuntime (previously dead code — implemented
// and tested but with no production caller) its first real caller.
func nodeVersionLine(ctx context.Context, projectRoot string) string {
	requiredMajor, err := config.RequiredNodeMajor(projectRoot)
	if err != nil {
		return "  node          - not configured"
	}
	nodePath, lookErr := exec.LookPath("node")
	if lookErr != nil {
		return fmt.Sprintf("  node          ! not found on PATH (requires %d from .nvmrc)", requiredMajor)
	}
	if err := config.ValidateNodeRuntime(projectRoot); err != nil {
		return fmt.Sprintf("  node          ! %v (resolved %s)", err, nodePath)
	}
	versionOut, vErr := exec.CommandContext(ctx, nodePath, "--version").Output()
	if vErr != nil {
		return fmt.Sprintf("  node          ! could not read node --version: %v (resolved %s)", vErr, nodePath)
	}
	return fmt.Sprintf("  node          ✓ %s (requires %d from .nvmrc, resolved %s)", strings.TrimSpace(string(versionOut)), requiredMajor, nodePath)
}

// serverVersionLine reports the running daemon's version against the CLI's
// own version, or "- not running" when no daemon is reachable. It never
// opens a new connection beyond the short-lived Health check below, so it
// cannot interfere with a real client's session.
func serverVersionLine(ctx context.Context) string {
	cli, err := connectClient(localserver.StateDir(), doctorTimeout)
	if err != nil {
		return "  server        - not running"
	}
	defer func() { _ = cli.Close() }()

	healthCtx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	resp, err := cli.Health(healthCtx)
	if err != nil {
		return "  server        - not running"
	}
	serverVersion := resp.GetServerVersion()
	if serverVersion == "" {
		return "  server        - unknown (dev build)"
	}
	if serverVersion == version {
		return fmt.Sprintf("  server        ✓ %s", serverVersion)
	}
	return fmt.Sprintf("  server        ! %s (differs from bridgectl %s — restart with bridgectl server stop && bridgectl server start)", serverVersion, version)
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
