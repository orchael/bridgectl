package localserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
)

const testValidControlCredential = "bri_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"

func statusFilePath(stateDir string) string {
	return filepath.Join(stateDir, "bridge-control-status.json")
}

// TestStart_NoControlConfig_NoControlClient guards the "no Bridge
// configuration means no control connection is attempted" invariant for
// standalone bridgectl (no Bridge enrollment at all): with no control:
// block in the config file, Start must never construct a control client or
// write a status file.
func TestStart_NoControlConfig_NoControlClient(t *testing.T) {
	stateDir := t.TempDir()
	srv, err := Start(Config{StateDir: stateDir, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	if _, err := os.Stat(statusFilePath(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("expected no control status file, got err=%v", err)
	}
}

// TestStart_OldEnrollmentConfig_NoControlClient covers an enrollment created
// before Bridge PR #16: the config file has a telemetry: block but no
// control: block at all. Control must be silently disabled, never fail
// startup.
func TestStart_OldEnrollmentConfig_NoControlClient(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(configPath, []byte("telemetry:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Config{StateDir: stateDir, ConfigPath: configPath, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	if _, err := os.Stat(statusFilePath(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("expected no control status file for a pre-control-plane enrollment, got err=%v", err)
	}
}

// TestStart_ControlConfig_MissingCredentialFile_DisablesControl covers a
// config: block pointing at a credential file that was never written (e.g.
// deleted out from under the daemon): Start must still succeed.
func TestStart_ControlConfig_MissingCredentialFile_DisablesControl(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bridge.yaml")
	yaml := "control:\n  endpoint: wss://control.bridge.example/v1/control\n  credential_file: " + filepath.Join(stateDir, "does-not-exist.json") + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Config{StateDir: stateDir, ConfigPath: configPath, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	if _, err := os.Stat(statusFilePath(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("expected no control status file when the credential file is missing, got err=%v", err)
	}
}

// TestStart_ControlConfig_MalformedCredential_DisablesControl covers a
// credential file whose control_credential does not match the bri_ format
// (including a brc_ telemetry credential mistakenly placed there): control
// must never authenticate with anything but a valid bri_ credential.
func TestStart_ControlConfig_MalformedCredential_DisablesControl(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bridge.yaml")
	credPath := filepath.Join(stateDir, "bridge-credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"collector_credential":"brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v","control_credential":"brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := "control:\n  endpoint: wss://control.bridge.example/v1/control\n  credential_file: " + credPath + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Config{StateDir: stateDir, ConfigPath: configPath, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	if _, err := os.Stat(statusFilePath(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("expected no control status file for a brc_-prefixed credential, got err=%v", err)
	}
}

// TestStart_ControlConfig_ValidCredential_ConnectsAsynchronously is the core
// architectural invariant test: Start must return promptly even though the
// configured control endpoint is unreachable, and the control client must
// still be running in the background (visible via the status file) rather
// than having blocked or failed local daemon startup.
func TestStart_ControlConfig_ValidCredential_ConnectsAsynchronously(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bridge.yaml")
	credPath := filepath.Join(stateDir, "bridge-credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"collector_credential":"brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v","control_credential":"`+testValidControlCredential+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// An address nothing listens on: the dial will fail, but Start() must not
	// wait for it.
	yaml := "control:\n  endpoint: wss://127.0.0.1:1/v1/control\n  credential_file: " + credPath + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	srv, err := Start(Config{StateDir: stateDir, ConfigPath: configPath, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Start took %v with an unreachable control endpoint, want near-instant (non-blocking)", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if st, err := bridgecontrol.ReadStatus(statusFilePath(stateDir)); err == nil {
			if st.State == bridgecontrol.StateConnecting || st.State == bridgecontrol.StateDisconnected || st.State == bridgecontrol.StateUnavailable {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("control status file never reflected a background connection attempt (lastErr=%v)", lastErr)
}

// TestStart_ControlFailing_SessionOperationsStillWork guards the
// architectural invariant that a *actively erroring* control client (not
// merely an absent one) never affects local Supervisor operations: starting
// and completing a real session must succeed and complete normally while
// the control client sits in a permanent, fast-retrying failure loop
// against an unreachable endpoint.
func TestStart_ControlFailing_SessionOperationsStillWork(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bridge.yaml")
	credPath := filepath.Join(stateDir, "bridge-credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"collector_credential":"brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v","control_credential":"`+testValidControlCredential+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `
control:
  endpoint: wss://127.0.0.1:1/v1/control
  credential_file: ` + credPath + `
providers:
  testprovider:
    binary: "cat"
    startup_probe: "none"
`
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, err := Start(Config{StateDir: stateDir, ConfigPath: configPath, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	// Give the control client a moment to actually start failing (not just
	// be absent), so this exercises "erroring", not "unconfigured".
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := bridgecontrol.ReadStatus(statusFilePath(stateDir)); err == nil && st.State != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := srv.supervisor.Start(ctx, bridge.SessionConfig{
		ProjectID: "p1", SessionID: "session-ops-during-control-failure", RepoPath: t.TempDir(),
		Options: map[string]string{"provider": "testprovider"},
	})
	if err != nil {
		t.Fatalf("session Start failed while control was erroring: %v", err)
	}
	if info.State != bridge.SessionStateRunning {
		t.Fatalf("session state = %v, want Running", info.State)
	}
	if err := srv.supervisor.Stop(info.SessionID, true); err != nil {
		t.Fatalf("session Stop failed while control was erroring: %v", err)
	}
}

// TestStart_ControlFailing_TelemetryStillDelivers guards the independence
// of the two outbound paths: telemetry event capture/delivery must keep
// working even while the separate control-plane connection is permanently
// failing. Telemetry here uses its local-spool destination (no
// collector_url/collector_target configured) rather than a real HTTPS
// collector: internal/config strictly validates collector_url as HTTPS at
// load time, and internal/telemetry's HTTP sink uses a fixed http.Client
// with no test-injectable transport, so a real TLS round trip isn't
// practical to assert on here without changing production code. The local
// spool still exercises the exact same Supervisor -> TelemetryObserver ->
// LiveCollector -> Sink pipeline this test is about — the write to disk
// happens synchronously with Record(), independent of the collector_url
// path added on top of it. Real end-to-end HTTPS delivery alongside a real
// control-plane failure was additionally verified live against
// bridge.orchael.dev / control.bridge.orchael.dev (see MAR-71 final
// report), where telemetry kept flowing throughout a control outage.
func TestStart_ControlFailing_TelemetryStillDelivers(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "bridge.yaml")
	credPath := filepath.Join(stateDir, "bridge-credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"collector_credential":"brc_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0U1v","control_credential":"`+testValidControlCredential+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := `
telemetry:
  enabled: true
control:
  endpoint: wss://127.0.0.1:1/v1/control
  credential_file: ` + credPath + `
providers:
  testprovider:
    binary: "cat"
    startup_probe: "none"
`
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, err := Start(Config{StateDir: stateDir, ConfigPath: configPath, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	// Confirm control really is failing (not merely slow to start), so this
	// test proves independence, not a race that happened not to matter.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := bridgecontrol.ReadStatus(statusFilePath(stateDir)); err == nil && st.State != "" && st.State != bridgecontrol.StateConnected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := srv.supervisor.Start(ctx, bridge.SessionConfig{
		ProjectID: "p1", SessionID: "telemetry-during-control-failure", RepoPath: t.TempDir(),
		Options: map[string]string{"provider": "testprovider"},
	}); err != nil {
		t.Fatalf("session Start: %v", err)
	}

	spoolFile := filepath.Join(TelemetrySpoolDir("", stateDir), ".active.jsonl")
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(spoolFile); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("telemetry never recorded a segment at %s while control was failing", spoolFile)
}
