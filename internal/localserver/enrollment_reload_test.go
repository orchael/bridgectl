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
	// EffectiveAllowedPaths (issue #238) makes $HOME the default session
	// allow-list entry, so the session's RepoPath must live under a $HOME
	// this test controls rather than an unrelated t.TempDir().
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	repoPath := filepath.Join(fakeHome, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}

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
	if _, err := srv.supervisor.Start(ctx, bridge.SessionConfig{SessionID: "live", ProjectID: "test", RepoPath: repoPath, Options: map[string]string{"provider": "testprovider"}}); err != nil {
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

// TestEnrollmentReloadOverwritesStaleStatusBeforeAck reproduces a Copilot
// review finding on PR #268: control.Start only schedules run()'s goroutine
// and returns immediately, so a stale "connected" entry left by a previous
// connection could still be on disk at the moment this reload's ack is
// written — letting waitForBridgeControl report success (or a stale
// rejection) without the new client ever attempting its own handshake.
// reloadEnrollment must synchronously clear that stale entry before Start.
func TestEnrollmentReloadOverwritesStaleStatusBeforeAck(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(configPath, []byte("providers: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Config{StateDir: dir, ConfigPath: configPath, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	statusPath := statusFilePath(dir)
	stale := bridgecontrol.Status{State: bridgecontrol.StateConnected, InstallationID: "stale-installation", UpdatedAt: time.Now()}
	if err := bridgecontrol.WriteStatus(statusPath, stale); err != nil {
		t.Fatal(err)
	}

	credPath := filepath.Join(dir, "bridge-credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"control_credential":"`+testValidControlCredential+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("control:\n  endpoint: wss://127.0.0.1:1/v1/control\n  credential_file: "+credPath+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	beforeReload := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ReloadEnrollment(ctx, dir, configPath); err != nil {
		t.Fatal(err)
	}

	st, err := bridgecontrol.ReadStatus(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.State == bridgecontrol.StateConnected {
		t.Fatalf("stale connected status survived reload: %+v", st)
	}
	if !st.UpdatedAt.After(beforeReload) {
		t.Fatalf("status was not rewritten synchronously during reload: %+v (reload started %v)", st, beforeReload)
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
