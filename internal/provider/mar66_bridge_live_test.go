package provider

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
)

type liveNoUpdateProvider struct{ *CodexAppServerProvider }

func (p *liveNoUpdateProvider) BuildCommand(ctx context.Context, cfg bridge.SessionConfig) (*exec.Cmd, error) {
	cmd, err := p.CodexAppServerProvider.BuildCommand(ctx, cfg)
	if err == nil {
		cmd.Args = append(cmd.Args, "-c", "check_for_update_on_startup=false")
	}
	return cmd, err
}

// This acceptance test deliberately leaves the response to the real Bridge
// browser. It starts a real Supervisor/TUI, releases the bootstrap writer,
// and fails unless provider evidence confirms that a remote response resumed
// the agent. Credentials and development targets are supplied explicitly.
func TestMAR66BridgeLive(t *testing.T) {
	if os.Getenv("MAR66_BRIDGE_LIVE") != "1" {
		t.Skip("requires real Bridge development installation and browser")
	}
	path := os.Getenv("MAR66_CREDENTIAL_FILE")
	if path == "" {
		t.Fatal("MAR66_CREDENTIAL_FILE required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var credentials struct {
		Token string `json:"control_credential"`
	}
	if json.Unmarshal(raw, &credentials) != nil || credentials.Token == "" {
		t.Fatal("control credential required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p := &liveNoUpdateProvider{newTestCodexAppServerProvider("codex")}
	registry := bridge.NewRegistry()
	if err := registry.Register(p); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	var sup *bridge.Supervisor
	var dispatches atomic.Int32
	client := bridgecontrol.New(bridgecontrol.Config{Endpoint: "wss://control.bridge.orchael.dev/v1/control", Credential: credentials.Token, CommandPath: filepath.Join(state, "commands"), RevisionPath: filepath.Join(state, "revisions"), SnapshotFunc: func() []bridgecontrol.SessionSnapshot { return bridgecontrol.ActiveSnapshots(sup.List("")) }, RespondFunc: func(c context.Context, s, p, text string) error {
		dispatches.Add(1)
		return sup.RespondToInput(c, s, p, text)
	}})
	sup = bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 65536, 0, bridge.WithControlObserver(bridgecontrol.NewSupervisorObserver(client)))
	client.Start(ctx)
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sup.Shutdown(c)
	}()
	sid := "mar66-live-" + uuid.NewString()
	initial, _ := json.Marshal(map[string]string{"sessionId": sid, "state": "starting"})
	if err := os.WriteFile("/tmp/mar66-bridge-live.json", initial, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(ctx, bridge.SessionConfig{SessionID: sid, ProjectID: "orchael/bridge", RepoPath: t.TempDir(), Options: map[string]string{"provider": "codex-app-server"}, InitialRows: 40, InitialCols: 120}); err != nil {
		t.Fatal(err)
	}
	attached, err := sup.Attach(sid, "acceptance-bootstrap", 0, bridge.AttachRoleWriter)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile("/tmp/mar66-bridge-tui.log", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	go func() {
		for chunk := range attached.Live {
			_, _ = log.Write(chunk.Payload)
		}
	}()
	time.Sleep(8 * time.Second)
	send := func(text string) {
		t.Helper()
		if _, err := sup.WriteInput(sid, "acceptance-bootstrap", []byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	send("\r")
	time.Sleep(2 * time.Second)
	send("/plan\r")
	time.Sleep(2 * time.Second)
	send("\x1b[200~Use request_user_input to ask exactly one question: Should the test marker be BLUE or GREEN? Do not inspect files or execute commands. Wait for my answer.\x1b[201~")
	time.Sleep(time.Second)
	send("\r")
	// Codex defers thread creation until its first prompt. A pasted prompt can
	// stay in its input buffer during startup; submit only while no thread has
	// been observed, before releasing the bootstrap writer.
	for attempt := 0; attempt < 5; attempt++ {
		time.Sleep(time.Second)
		info, _ := sup.Get(sid)
		if info.Interaction.State != bridge.InteractionUnknown {
			break
		}
		send("\r")
	}
	if _, err := sup.Detach(sid, "acceptance-bootstrap"); err != nil {
		t.Fatal(err)
	}
	t.Logf("real bridgectl session %s; bootstrap writer released", sid)
	waiting, resumed := false, false
	previous := ""
	for {
		info, err := sup.Get(sid)
		if err != nil {
			t.Fatal(err)
		}
		state := string(info.Interaction.State)
		if state != previous {
			t.Logf("provider state=%s pending=%+v", state, info.Interaction.Pending)
			previous = state
		}
		if !waiting && info.Interaction.State == bridge.InteractionWaitingForInput && info.Interaction.Pending != nil {
			waiting = true
			b, _ := json.Marshal(map[string]any{"sessionId": sid, "pendingRequestId": info.Interaction.Pending.ID, "state": "waiting_for_input"})
			if err := os.WriteFile("/tmp/mar66-bridge-live.json", b, 0600); err != nil {
				t.Fatal(err)
			}
			t.Log("waiting for a response through https://bridge.orchael.dev/sessions")
		}
		if waiting && info.Interaction.State == bridge.InteractionWorking && info.Interaction.Pending == nil {
			resumed = true
		}
		if resumed && info.Interaction.State == bridge.InteractionIdle {
			t.Log("remote response acknowledged by provider; pending cleared; idle")
			time.Sleep(35 * time.Second) // allow the browser to observe the live transition
			if dispatches.Load() != 1 {
				t.Fatalf("response dispatch count=%d, want 1", dispatches.Load())
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("Bridge response cycle did not finish")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
