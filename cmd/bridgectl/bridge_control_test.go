package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridgecontrol"
	"github.com/orchael/bridgectl/internal/localserver"
)

const validControlCredential = "bri_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"
const validCollectorCredential = "brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"

func newControlDeviceToken() deviceToken {
	return deviceToken{
		BridgeURL:           "https://bridge.example",
		APIVersion:          "v1",
		OrganizationID:      "org",
		InstallationID:      "install",
		TelemetryEndpoint:   "https://bridge.example/v1/telemetry/segments",
		CollectorCredential: validCollectorCredential,
		SchemaVersion:       2,
		ControlEndpoint:     "wss://control.bridge.example/v1/control",
		ControlCredential:   validControlCredential,
	}
}

func TestPersistBridgeEnrollment_StoresControlFieldsWhenSchemaVersion2(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := persistBridgeEnrollment(newControlDeviceToken(), "laptop", "https://bridge.example"); err != nil {
		t.Fatal(err)
	}
	e, err := readEnrollmentMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if e.SchemaVersion != 2 || e.ControlEndpoint != "wss://control.bridge.example/v1/control" {
		t.Fatalf("unexpected enrollment: %+v", e)
	}
	s, err := readBridgeSecret()
	if err != nil {
		t.Fatal(err)
	}
	if s.ControlCredential != validControlCredential {
		t.Fatalf("control credential not persisted: %+v", s)
	}
	if !controlProvisioned(e, s.ControlCredential) {
		t.Fatal("expected controlProvisioned to be true")
	}

	b, err := os.ReadFile(bridgeConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "endpoint: wss://control.bridge.example/v1/control") {
		t.Fatalf("bridge.yaml missing control endpoint: %s", b)
	}
	if !strings.Contains(string(b), "managed_by_bridge: true") {
		t.Fatalf("bridge.yaml control block not marked managed_by_bridge: %s", b)
	}
}

// TestPersistBridgeEnrollment_OldEnrollmentHasNoControlFields covers logging
// into a Bridge deployment that predates PR #16: the response has no
// schema_version/control_endpoint/control_credential, and enrollment must
// still succeed as a valid Bridge/telemetry-only enrollment.
func TestPersistBridgeEnrollment_OldEnrollmentHasNoControlFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: validCollectorCredential}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err != nil {
		t.Fatal(err)
	}
	e, err := readEnrollmentMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if e.SchemaVersion != 0 || e.ControlEndpoint != "" {
		t.Fatalf("expected no control fields on an old enrollment, got %+v", e)
	}
	s, err := readBridgeSecret()
	if err != nil {
		t.Fatal(err)
	}
	if s.ControlCredential != "" {
		t.Fatalf("expected no control credential on an old enrollment, got %+v", s)
	}
	if controlProvisioned(e, s.ControlCredential) {
		t.Fatal("expected controlProvisioned to be false for an old enrollment")
	}
	if b, err := os.ReadFile(bridgeConfigPath()); err == nil && strings.Contains(string(b), "control:") {
		t.Fatalf("expected no control: block for an old enrollment, got %s", b)
	}
}

func TestPersistBridgeEnrollment_RejectsIncompleteControlFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := newControlDeviceToken()
	tok.ControlEndpoint = ""
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err == nil {
		t.Fatal("expected an error when schema_version >= 2 but control_endpoint is missing")
	}
}

func TestPersistBridgeEnrollment_RejectsMalformedControlCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for name, cred := range map[string]string{
		"telemetry credential reused for control": validCollectorCredential,
		"wrong length": "bri_tooshort",
		"empty":        "",
	} {
		t.Run(name, func(t *testing.T) {
			tok := newControlDeviceToken()
			tok.ControlCredential = cred
			if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err == nil {
				t.Fatalf("expected control credential %q to be rejected", cred)
			}
		})
	}
}

func TestPersistBridgeEnrollment_RejectsNonWssControlEndpoint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for name, endpoint := range map[string]string{
		"plain ws":     "ws://control.bridge.example/v1/control",
		"https":        "https://control.bridge.example/v1/control",
		"no path":      "wss://control.bridge.example",
		"has query":    "wss://control.bridge.example/v1/control?x=1",
		"has userinfo": "wss://user@control.bridge.example/v1/control",
	} {
		t.Run(name, func(t *testing.T) {
			tok := newControlDeviceToken()
			tok.ControlEndpoint = endpoint
			if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err == nil {
				t.Fatalf("expected control endpoint %q to be rejected", endpoint)
			}
		})
	}
}

func TestControlProvisioned_NilSafety(t *testing.T) {
	if controlProvisioned(nil, "") {
		t.Fatal("nil enrollment must not be provisioned")
	}
	e := &bridgeEnrollment{SchemaVersion: 2, ControlEndpoint: "wss://x/v1/control"}
	if controlProvisioned(e, "") {
		t.Fatal("empty control credential must not be provisioned")
	}
	if controlProvisioned(e, validCollectorCredential) {
		t.Fatal("a brc_ telemetry credential must never satisfy control provisioning")
	}
}

func TestWhoami_ShowsControlAndTelemetryStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := persistBridgeEnrollment(newControlDeviceToken(), "laptop", "https://bridge.example"); err != nil {
		t.Fatal(err)
	}
	cmd := newBridgeWhoamiCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Telemetry       configured") {
		t.Fatalf("missing telemetry status: %s", out.String())
	}
	if !strings.Contains(out.String(), "Control         configured") {
		t.Fatalf("missing control status: %s", out.String())
	}
}

func TestWhoami_ShowsControlNotConfiguredForOldEnrollment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := deviceToken{BridgeURL: "https://bridge.example", APIVersion: "v1", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", CollectorCredential: validCollectorCredential}
	if err := persistBridgeEnrollment(tok, "laptop", "https://bridge.example"); err != nil {
		t.Fatal(err)
	}
	cmd := newBridgeWhoamiCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Control         not configured") {
		t.Fatalf("expected control not configured for an old enrollment: %s", out.String())
	}
}

func TestDoctor_ControlNotProvisioned(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mp, _ := bridgeStatePaths()
	if err := atomicJSON(mp, bridgeEnrollment{BridgeURL: "https://bridge.example", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments"}); err != nil {
		t.Fatal(err)
	}
	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "control       - not provisioned — run bridgectl bridge login --force") {
		t.Fatalf("expected not-provisioned control line: %s", out.String())
	}
}

func TestDoctor_ControlConnectedFromStatusFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mp, sp := bridgeStatePaths()
	if err := atomicJSON(mp, bridgeEnrollment{BridgeURL: "https://bridge.example", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", SchemaVersion: 2, ControlEndpoint: "wss://control.bridge.example/v1/control"}); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(sp, bridgeSecret{CollectorCredential: validCollectorCredential, ControlCredential: validControlCredential}); err != nil {
		t.Fatal(err)
	}
	if err := bridgecontrol.WriteStatus(dir+"/bridge-control-status.json", bridgecontrol.Status{State: bridgecontrol.StateConnected, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if localserver.StateDir() != dir {
		t.Fatalf("StateDir() = %q, want %q (BRIDGECTL_STATE_DIR not honored)", localserver.StateDir(), dir)
	}
	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "control       ✓ connected") {
		t.Fatalf("expected connected control line: %s", out.String())
	}
}

// TestDoctor_ControlStaleConnectedStatusReportsDisconnected guards against
// trusting a persisted "connected" state indefinitely: if the daemon died
// without a chance to write a final status (killed, powered off), a stale
// "connected" entry must not be reported as live forever.
func TestDoctor_ControlStaleConnectedStatusReportsDisconnected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mp, sp := bridgeStatePaths()
	if err := atomicJSON(mp, bridgeEnrollment{BridgeURL: "https://bridge.example", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", SchemaVersion: 2, ControlEndpoint: "wss://control.bridge.example/v1/control"}); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(sp, bridgeSecret{CollectorCredential: validCollectorCredential, ControlCredential: validControlCredential}); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().UTC().Add(-2 * bridgecontrol.StatusStaleAfter)
	if err := bridgecontrol.WriteStatus(dir+"/bridge-control-status.json", bridgecontrol.Status{State: bridgecontrol.StateConnected, UpdatedAt: stale}); err != nil {
		t.Fatal(err)
	}
	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "control       ! disconnected") {
		t.Fatalf("expected a stale connected status to report disconnected: %s", out.String())
	}
}

// TestWhoamiAndDoctor_ControlIndependentOfBrokenTelemetryCredential guards
// against conflating the two unrelated credentials in the same secret
// file: a malformed collector_credential (telemetry) must never make an
// otherwise-valid control_credential report as unconfigured/unprovisioned,
// since the daemon reads and uses them independently.
func TestWhoamiAndDoctor_ControlIndependentOfBrokenTelemetryCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mp, sp := bridgeStatePaths()
	if err := atomicJSON(mp, bridgeEnrollment{BridgeURL: "https://bridge.example", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", SchemaVersion: 2, ControlEndpoint: "wss://control.bridge.example/v1/control"}); err != nil {
		t.Fatal(err)
	}
	// CollectorCredential is deliberately malformed; ControlCredential is
	// valid.
	if err := atomicJSON(sp, bridgeSecret{CollectorCredential: "not-a-valid-credential", ControlCredential: validControlCredential}); err != nil {
		t.Fatal(err)
	}
	if err := bridgecontrol.WriteStatus(dir+"/bridge-control-status.json", bridgecontrol.Status{State: bridgecontrol.StateConnected, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	whoamiCmd := newBridgeWhoamiCmd()
	var whoamiOut bytes.Buffer
	whoamiCmd.SetOut(&whoamiOut)
	if err := whoamiCmd.RunE(whoamiCmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(whoamiOut.String(), "Telemetry       not configured") {
		t.Fatalf("expected telemetry to report not configured: %s", whoamiOut.String())
	}
	if !strings.Contains(whoamiOut.String(), "Control         configured") {
		t.Fatalf("expected control to remain configured despite a broken telemetry credential: %s", whoamiOut.String())
	}

	doctorCmd := newDoctorCmd()
	var doctorOut bytes.Buffer
	doctorCmd.SetOut(&doctorOut)
	if err := doctorCmd.RunE(doctorCmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doctorOut.String(), "telemetry     ! credential missing") {
		t.Fatalf("expected telemetry credential missing: %s", doctorOut.String())
	}
	if !strings.Contains(doctorOut.String(), "control       ✓ connected") {
		t.Fatalf("expected control to remain connected despite a broken telemetry credential: %s", doctorOut.String())
	}
}

func TestDoctor_ControlDisconnectedWhenNoStatusFileYet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mp, sp := bridgeStatePaths()
	if err := atomicJSON(mp, bridgeEnrollment{BridgeURL: "https://bridge.example", OrganizationID: "org", InstallationID: "install", TelemetryEndpoint: "https://bridge.example/v1/telemetry/segments", SchemaVersion: 2, ControlEndpoint: "wss://control.bridge.example/v1/control"}); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(sp, bridgeSecret{CollectorCredential: validCollectorCredential, ControlCredential: validControlCredential}); err != nil {
		t.Fatal(err)
	}
	cmd := newDoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "control       ! disconnected") {
		t.Fatalf("expected disconnected control line: %s", out.String())
	}
}

func TestLogout_RemovesManagedControlBlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := persistBridgeEnrollment(newControlDeviceToken(), "laptop", "https://bridge.example"); err != nil {
		t.Fatal(err)
	}
	cmd := newBridgeLogoutCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(bridgeConfigPath())
	if err == nil && strings.Contains(string(b), "control:") {
		t.Fatalf("expected managed control: block removed on logout, got %s", b)
	}
}
