package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orchael/bridgectl/internal/localserver"
)

func TestBridgeReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()
	if !bridgeReachable(server.URL) {
		t.Fatal("expected reachable server to report reachable")
	}
	if bridgeReachable("https://127.0.0.1:1") {
		t.Fatal("expected unroutable address to report unreachable")
	}
}

// TestDoctorReportsNetworkStatus guards the doctor checklist requirement to
// distinguish network availability from enrollment/credential status, not
// just report local file state.
func TestDoctorReportsNetworkStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := deviceToken{BridgeURL: server.URL, APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}
	if err := atomicJSON(func() string { mp, _ := bridgeStatePaths(); return mp }(), bridgeEnrollment{BridgeURL: tok.BridgeURL, OrganizationID: tok.OrganizationID, InstallationID: tok.InstallationID, TelemetryEndpoint: tok.TelemetryEndpoint}); err != nil {
		t.Fatal(err)
	}
	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	// No telemetry credential file is written in this fixture, so doctor's
	// "!" exit (issue #238) is expected; this test is about the network
	// line specifically, not overall doctor health.
	_ = cmd.RunE(cmd, nil)
	if !strings.Contains(out.String(), "network       ✓ reachable") {
		t.Fatalf("expected reachable network status: %s", out.String())
	}
}

// writeFakeNode puts an executable "node" script on a PATH-only directory
// that prints versionOutput for "node --version", and points PATH at it for
// the duration of the test. This keeps the check deterministic regardless
// of whatever Node (if any) happens to be installed on the host or CI
// runner, matching the issue #265 acceptance criterion that version checks
// must not depend on installed providers.
func writeFakeNode(t *testing.T, versionOutput string) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\necho " + versionOutput + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "node"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake node: %v", err)
	}
	t.Setenv("PATH", binDir)
}

// TestDoctorProviderRootFallsBackToXDGConfig guards against doctor checking
// only <stateDir>/bridge.yaml while `server start` also auto-loads
// $XDG_CONFIG_HOME/bridgectl/config.yaml: without the fix, doctor would read
// ./.nvmrc (CWD) instead of the daemon's actual runtime.provider_root and
// report a false Node status. Flagged by Copilot review on PR #272.
func TestDoctorProviderRootFallsBackToXDGConfig(t *testing.T) {
	stateDir := t.TempDir() // no bridge.yaml here
	t.Setenv("BRIDGECTL_STATE_DIR", stateDir)

	xdgConfigHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	bridgectlConfigDir := filepath.Join(xdgConfigHome, "bridgectl")
	if err := os.MkdirAll(bridgectlConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerRoot := t.TempDir()
	configYAML := "runtime:\n  provider_root: " + providerRoot + "\n"
	if err := os.WriteFile(filepath.Join(bridgectlConfigDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	got := doctorProviderRoot()
	if got != providerRoot {
		t.Fatalf("doctorProviderRoot() = %q, want %q", got, providerRoot)
	}
}

func TestNodeVersionLineNotConfigured(t *testing.T) {
	dir := t.TempDir() // no .nvmrc
	got := nodeVersionLine(context.Background(), dir)
	if got != "  node          - not configured" {
		t.Fatalf("got %q", got)
	}
}

func TestNodeVersionLineNotFoundOnPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".nvmrc"), []byte("24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // empty: no "node" binary anywhere on PATH
	got := nodeVersionLine(context.Background(), dir)
	want := "  node          ! not found on PATH (requires 24 from .nvmrc)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestNodeVersionLinePass(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".nvmrc"), []byte("24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeNode(t, "v24.1.0")
	got := nodeVersionLine(context.Background(), dir)
	if !strings.HasPrefix(got, "  node          ✓ v24.1.0 (requires 24 from .nvmrc, resolved ") {
		t.Fatalf("got %q", got)
	}
}

func TestNodeVersionLineMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".nvmrc"), []byte("24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeNode(t, "v20.0.0")
	got := nodeVersionLine(context.Background(), dir)
	want := "  node          ! node on PATH is v20 but bridge requires v24 from .nvmrc"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("got %q want prefix %q", got, want)
	}
}

func TestServerVersionLineNotRunning(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	got := serverVersionLine(context.Background())
	if got != "  server        - not running" {
		t.Fatalf("got %q", got)
	}
}

func TestServerVersionLineMatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	srv, err := localserver.Start(localserver.Config{StateDir: dir, Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	oldVersion := version
	version = "1.2.3"
	defer func() { version = oldVersion }()

	got := serverVersionLine(context.Background())
	if got != "  server        ✓ 1.2.3" {
		t.Fatalf("got %q", got)
	}
}

func TestServerVersionLineMismatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	srv, err := localserver.Start(localserver.Config{StateDir: dir, Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	oldVersion := version
	version = "1.4.0"
	defer func() { version = oldVersion }()

	got := serverVersionLine(context.Background())
	want := "  server        ! 1.2.3 (differs from bridgectl 1.4.0"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("got %q want prefix %q", got, want)
	}
}

// TestDoctorExitsZeroWhenHealthy covers issue #238's "doctor exits 0 on
// success, 1 on any failure" acceptance criterion: a bare environment with
// no enrollment and no running daemon has only "✓"/"-" markers (never "!"),
// so doctor must report success.
func TestDoctorExitsZeroWhenHealthy(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("expected a healthy report to exit cleanly, got %v: %s", err, out.String())
	}
}

// TestDoctorExitsNonZeroOnFailure is the failure-path complement: a
// malformed telemetry credential produces a "!" finding, so doctor must
// report failure.
func TestDoctorExitsNonZeroOnFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	mp, _ := bridgeStatePaths()
	if err := atomicJSON(mp, bridgeEnrollment{BridgeURL: "https://bridge.example", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments"}); err != nil {
		t.Fatal(err)
	}

	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatalf("expected a report with a \"!\" finding to fail, got nil: %s", out.String())
	}
}

// TestDoctorJSONOutput covers the --json acceptance criterion: the flag
// must emit a single parseable JSON object whose findings decompose the
// same report sections the text mode prints.
func TestDoctorJSONOutput(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("expected a healthy report to exit cleanly, got %v: %s", err, out.String())
	}

	var report struct {
		OK       bool `json:"ok"`
		Findings []struct {
			Section string `json:"section"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("doctor --json did not emit parseable JSON: %v\n%s", err, out.String())
	}
	if !report.OK {
		t.Fatalf("expected ok=true for a healthy report: %+v", report)
	}
	if len(report.Findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	var sawVersions, sawAccess bool
	for _, f := range report.Findings {
		if f.Status == "" || f.Message == "" {
			t.Fatalf("finding missing status or message: %+v", f)
		}
		switch f.Section {
		case "Versions":
			sawVersions = true
		case "Access":
			sawAccess = true
		}
	}
	if !sawVersions || !sawAccess {
		t.Fatalf("expected Versions and Access sections in findings: %+v", report.Findings)
	}
}

// TestAccessSectionLinesIncludesHome covers issue #238's "doctor shows the
// effective allowed-path list, including the implicit $HOME entry".
func TestAccessSectionLinesIncludesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	lines := accessSectionLines()
	if len(lines) != 1 || !strings.Contains(lines[0], home) {
		t.Fatalf("expected the allowed-paths line to include $HOME %q, got %v", home, lines)
	}
}

// TestAccessSectionLinesIsAdditiveWithConfig covers the "explicit
// allowed_paths entries remain additive" acceptance criterion: a
// configured allowed_paths entry must appear alongside $HOME, not instead
// of it.
func TestAccessSectionLinesIsAdditiveWithConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", stateDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configPath := filepath.Join(stateDir, "bridge.yaml")
	if err := os.WriteFile(configPath, []byte("allowed_paths:\n  - /srv/repos\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lines := accessSectionLines()
	if len(lines) != 1 || !strings.Contains(lines[0], home) || !strings.Contains(lines[0], "/srv/repos") {
		t.Fatalf("expected both $HOME %q and /srv/repos in the allowed-paths line, got %v", home, lines)
	}
}
