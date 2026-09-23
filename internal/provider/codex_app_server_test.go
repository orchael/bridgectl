package provider

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/codexapp"
)

func newTestCodexAppServerProvider(binary string) *CodexAppServerProvider {
	return NewCodexAppServerProvider(StdioConfig{
		ProviderID:   "codex-app-server",
		Binary:       binary,
		StartupProbe: "none",
	})
}

func TestCodexAppServer_IdentityAndCapabilities(t *testing.T) {
	p := newTestCodexAppServerProvider("codex")
	if p.ID() != "codex-app-server" {
		t.Fatalf("ID() = %q, want codex-app-server", p.ID())
	}
	if p.Binary() != "codex" {
		t.Fatalf("Binary() = %q, want codex", p.Binary())
	}
	caps := p.InteractionCapabilities()
	if caps != codexapp.Capabilities {
		t.Fatalf("InteractionCapabilities() = %+v, want exactly codexapp.Capabilities %+v", caps, codexapp.Capabilities)
	}
}

func TestCodexAppServer_WatchInteraction_UnknownSession(t *testing.T) {
	p := newTestCodexAppServerProvider("codex")
	_, err := p.WatchInteraction(context.Background(), "never-started", bridge.SessionConfig{})
	if err == nil {
		t.Fatal("expected an error watching a session BuildCommand never started")
	}
}

func TestCodexAppServer_BuildCommand_MissingBinary(t *testing.T) {
	p := newTestCodexAppServerProvider("bridgectl-definitely-not-a-real-binary")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := p.BuildCommand(ctx, bridge.SessionConfig{SessionID: "s1", RepoPath: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error for a nonexistent binary")
	}
}

func TestFreeLocalPort_ReturnsUsablePort(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 {
		t.Fatalf("port = %d, want > 0", port)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("port %d was not actually free: %v", port, err)
	}
	_ = l.Close()
}

func TestWaitForPort_SucceedsOnceListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !waitForPort(ctx, "127.0.0.1", port, 2*time.Second) {
		t.Fatal("waitForPort returned false for an already-listening port")
	}
}

func TestWaitForPort_TimesOutWhenNothingListens(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if waitForPort(ctx, "127.0.0.1", port, 300*time.Millisecond) {
		t.Fatal("waitForPort returned true for a port nothing is listening on")
	}
}

func TestWaitForPort_RespectsCancellation(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if waitForPort(ctx, "127.0.0.1", port, 10*time.Second) {
		t.Fatal("waitForPort returned true despite a cancelled context")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waitForPort did not return promptly on cancellation")
	}
}

// --- Real-binary integration test ---
//
// This exercises the genuine `codex` binary present in the environment
// (verified against the real app-server protocol during development — see
// docs/codex-app-server-observer.md). It never needs OpenAI/ChatGPT
// credentials: thread/start and an "idle" ThreadStatus are both available
// locally, before any real model call is attempted. It is skipped, not
// failed, when `codex` isn't on PATH (e.g. most CI runners).
func TestCodexAppServer_RealBinary_CompanionStartsAndObserverSeesIdle(t *testing.T) {
	binPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex binary not on PATH; skipping real app-server integration test")
	}

	p := newTestCodexAppServerProvider(binPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := bridge.SessionConfig{SessionID: "real-test-session", RepoPath: t.TempDir()}
	tuiCmd, err := p.BuildCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	if tuiCmd == nil {
		t.Fatal("BuildCommand returned a nil command")
	}
	// Deliberately never Start tuiCmd: this test verifies the companion
	// app-server + observer wiring, which needs no real TUI/terminal.
	// thread/start below stands in for what the real TUI would do.

	interactions, err := p.WatchInteraction(ctx, cfg.SessionID, cfg)
	if err != nil {
		t.Fatalf("WatchInteraction: %v", err)
	}

	p.mu.Lock()
	sess, ok := p.sessions[cfg.SessionID]
	p.mu.Unlock()
	if !ok {
		t.Fatal("companion app-server session was not recorded")
	}
	endpoint := "ws://127.0.0.1:" + strconv.Itoa(sess.port)

	simulateTUIThreadStart(t, endpoint)

	select {
	case got := <-interactions:
		if got.EffectiveState() != bridge.InteractionIdle && got.EffectiveState() != bridge.InteractionWorking {
			t.Fatalf("state = %q, want idle or working from a freshly created thread", got.EffectiveState())
		}
		if got.Evidence.Source != "codex-app-server" {
			t.Fatalf("Evidence.Source = %q, want codex-app-server", got.Evidence.Source)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("observer never reported an interaction state for the real thread")
	}
}

// simulateTUIThreadStart connects to the companion app-server exactly like
// the real `codex --remote` TUI would (a second client on the same
// endpoint) and calls thread/start, so the provider's observer connection
// has something real to see — without needing OpenAI/ChatGPT credentials,
// since thread creation and its initial "idle" status don't call the model.
func simulateTUIThreadStart(t *testing.T, wsURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("simulated TUI dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	send := func(id int, method string, params any) {
		b, marshalErr := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatalf("simulated TUI write: %v", err)
		}
	}
	readOne := func() map[string]any {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("simulated TUI read: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		return m
	}

	send(1, "initialize", map[string]any{"clientInfo": map[string]any{"name": "sim-tui", "version": "1"}})
	for {
		m := readOne()
		if _, hasMethod := m["method"]; !hasMethod {
			break // response to id 1
		}
	}
	send(2, "thread/start", map[string]any{"cwd": t.TempDir()})
	for {
		m := readOne()
		if _, hasMethod := m["method"]; !hasMethod {
			break // response to id 2
		}
	}
}

func TestCodexAppServer_CompanionEndpoint_UnknownSession(t *testing.T) {
	p := newTestCodexAppServerProvider("codex")
	if _, ok := p.CompanionEndpoint("never-started"); ok {
		t.Fatal("expected ok=false for a session BuildCommand never started")
	}
}
