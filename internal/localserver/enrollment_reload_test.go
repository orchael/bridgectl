package localserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
)

// Login after daemon startup must activate reporting without stopping sessions.
func TestEnrollmentReloadPreservesLiveSession(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(stateDir, "bridge.yaml")
	if err := os.WriteFile(configPath, []byte("providers:\n  testprovider:\n    binary: cat\n    startup_probe: none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Config{StateDir: stateDir, ConfigPath: configPath, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := srv.supervisor.Start(ctx, bridge.SessionConfig{SessionID: "live", ProjectID: "test", RepoPath: t.TempDir(), Options: map[string]string{"provider": "testprovider"}}); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(stateDir, "bridge-credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"control_credential":"`+testValidControlCredential+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("control:\n  endpoint: wss://127.0.0.1:1/v1/control\n  credential_file: "+credPath+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReloadEnrollment(ctx, stateDir, configPath); err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := bridgecontrol.ReadStatus(statusFilePath(stateDir)); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("running daemon never activated enrollment")
		case <-time.After(20 * time.Millisecond):
		}
	}
	infos := srv.supervisor.List("")
	if len(infos) != 1 || infos[0].ProcessID == 0 || infos[0].State == bridge.SessionStateStopped {
		t.Fatalf("reload lost live session: %+v", infos)
	}
}

func TestEnrollmentReloadReportsInvalidConfigAndCanRetry(t *testing.T) {
	dir := t.TempDir()
	srv, err := Start(Config{StateDir: dir, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	path := filepath.Join(dir, "new-config.yaml")
	if err := os.WriteFile(path, []byte("invalid: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReloadEnrollment(ctx, dir, path); err == nil || !strings.Contains(err.Error(), "load enrollment configuration") {
		t.Fatalf("invalid config error: %v", err)
	}
	if err := os.WriteFile(path, []byte("telemetry:\n  enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReloadEnrollment(ctx, dir, path); err != nil {
		t.Fatal(err)
	}
	st, err := bridgecontrol.ReadStatus(statusFilePath(dir))
	if err != nil || st.State != bridgecontrol.StateNotProvisioned {
		t.Fatalf("control should be disabled: %+v, %v", st, err)
	}
}

func TestEnrollmentReloadNoDaemonTimesOut(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := ReloadEnrollment(ctx, dir, filepath.Join(dir, "bridge.yaml")); err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("missing daemon error: %v", err)
	}
}
