package main

import (
	"context"
	"encoding/json"
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

// doctorSection is one named group of report lines (e.g. "Bridge",
// "Versions", "Access"). Each line keeps the existing "  <label>  <marker>
// <message>" text format used throughout doctor's output; jsonFindings
// parses that same text to build the --json representation instead of
// keeping a second, separately-maintained structured model in sync.
type doctorSection struct {
	name  string
	lines []string
}

// doctorFinding is one line of a doctorSection, decomposed for --json.
type doctorFinding struct {
	Section string `json:"section"`
	Status  string `json:"status"` // "ok" (✓), "warn" (!), or "unknown" (-)
	Message string `json:"message"`
}

func newDoctorCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{Use: "doctor", Short: "Check local bridgectl health", RunE: func(cmd *cobra.Command, _ []string) error {
		sections := []doctorSection{
			{name: "Bridge", lines: bridgeSectionLines()},
			{name: "Versions", lines: versionsSectionLines(cmd.Context())},
			{name: "Access", lines: accessSectionLines()},
		}
		findings := doctorFindings(sections)

		if jsonOutput {
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				OK       bool            `json:"ok"`
				Findings []doctorFinding `json:"findings"`
			}{OK: !anyWarnings(findings), Findings: findings}); err != nil {
				return err
			}
		} else {
			printDoctorText(cmd.OutOrStdout(), sections)
		}

		if anyWarnings(findings) {
			return errors.New("doctor: one or more checks reported a problem (see report above)")
		}
		return nil
	}}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the report as a single JSON object instead of human-readable text")
	return cmd
}

func printDoctorText(out io.Writer, sections []doctorSection) {
	for i, s := range sections {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		_, _ = fmt.Fprintln(out, s.name)
		for _, line := range s.lines {
			_, _ = fmt.Fprintln(out, line)
		}
	}
}

// doctorFindings decomposes every section's lines into the --json shape.
// Lines consistently follow "  <label (padded)><marker> <message>"; no
// label in this file contains '✓', '!', or '-', so the first occurrence of
// one of those runes in a line is always its status marker, never part of
// the label.
func doctorFindings(sections []doctorSection) []doctorFinding {
	var findings []doctorFinding
	for _, s := range sections {
		for _, line := range s.lines {
			findings = append(findings, doctorFinding{
				Section: s.name,
				Status:  lineStatus(line),
				Message: strings.TrimSpace(line),
			})
		}
	}
	return findings
}

func lineStatus(line string) string {
	for _, r := range line {
		switch r {
		case '✓':
			return "ok"
		case '!':
			return "warn"
		case '-':
			return "unknown"
		}
	}
	return "unknown"
}

func anyWarnings(findings []doctorFinding) bool {
	for _, f := range findings {
		if f.Status == "warn" {
			return true
		}
	}
	return false
}

// bridgeSectionLines reports the connectivity/enrollment status. An
// unreadable or missing enrollment file is itself a finding, reported
// inline, not a reason to abort the rest of doctor.
func bridgeSectionLines() []string {
	e, err := readEnrollmentMetadata()
	if errors.Is(err, os.ErrNotExist) {
		return []string{
			"  server        - not configured",
			"  enrollment    - not logged in",
		}
	}
	if err != nil {
		return []string{fmt.Sprintf("  enrollment    ! unreadable (%v)", err)}
	}

	lines := []string{fmt.Sprintf("  server        ✓ %s", e.BridgeURL)}
	if bridgeReachable(e.BridgeURL) {
		lines = append(lines, "  network       ✓ reachable")
	} else {
		lines = append(lines, "  network       ! unreachable")
	}
	lines = append(lines,
		"  enrollment    ✓ logged in",
		fmt.Sprintf("  organization  ✓ %s", display(e.OrganizationName, e.OrganizationID)),
	)
	if e.OrganizationURL != "" {
		lines = append(lines, fmt.Sprintf("  org url       ✓ %s", e.OrganizationURL))
	}
	lines = append(lines, fmt.Sprintf("  installation  ✓ %s", display(e.InstallationName, e.InstallationID)))

	secret, secretErr := readBridgeSecret()
	if secretErr != nil || secret == nil || secret.CollectorCredential == "" {
		lines = append(lines, "  telemetry     ! credential missing")
	} else {
		lines = append(lines, "  telemetry     ✓ configured")
	}
	controlCredential, _ := readControlCredential() // a read error just means "not usable" here
	lines = append(lines, "  control       "+controlDoctorLine(e, controlCredential))
	return lines
}

// versionsSectionLines reports the bridgectl, Node, and running-daemon
// versions doctor can check offline and without any installed provider.
// Provider CLI version drift and macOS launch-agent staleness are tracked
// separately (see issue #265) and intentionally left out of this section.
func versionsSectionLines(ctx context.Context) []string {
	return []string{
		fmt.Sprintf("  bridgectl     ✓ %s", version),
		nodeVersionLine(ctx, doctorProviderRoot()),
		serverVersionLine(ctx),
	}
}

// accessSectionLines reports the effective session allowed-path list,
// including the implicit $HOME entry every session may always use (see
// localserver.EffectiveAllowedPaths, issue #238). This is config-derived,
// not daemon-runtime state, so it is computed directly from the same
// config file doctor's Versions section already reads rather than adding
// another RPC round-trip.
func accessSectionLines() []string {
	configPath := defaultServerConfigPath(localserver.StateDir())
	var configured []string
	if configPath != "" {
		fileCfg, err := config.Load(configPath)
		if err != nil {
			// A discovered-but-unloadable config is a real problem: the
			// daemon would hit the same error on startup, so doctor must
			// not silently fall through and report success.
			return []string{fmt.Sprintf("  allowed paths ! could not load %s: %v", configPath, err)}
		}
		configured = fileCfg.AllowedPaths
	}
	effective, err := localserver.EffectiveAllowedPaths(configured)
	if err != nil {
		return []string{fmt.Sprintf("  allowed paths ! %v", err)}
	}
	return []string{fmt.Sprintf("  allowed paths ✓ %s", strings.Join(effective, ", "))}
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
	return fmt.Sprintf("  server        ! %s (differs from bridgectl %s — finish active sessions, then restart the service that owns the server; on Linux this may be bridge.service or bridgectl.service)", serverVersion, version)
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
