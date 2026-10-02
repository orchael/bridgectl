package provider

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/orchael/bridgectl/internal/bridge"
)

// Opt-in diagnostic: runs the real TUI and a real authenticated model turn.
func TestMAR66RealTUIWaiting(t *testing.T) {
	if os.Getenv("MAR66_LIVE") != "1" {
		t.Skip("set MAR66_LIVE=1 for authenticated Codex acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	p := newTestCodexAppServerProvider("codex")
	cfg := bridge.SessionConfig{SessionID: "mar66-live", RepoPath: t.TempDir()}
	cmd, err := p.BuildCommand(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args, "-c", "check_for_update_on_startup=false")
	updates, err := p.WatchInteraction(ctx, cfg.SessionID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = terminal.Close() }()
	defer func() { _ = cmd.Wait() }()
	defer cancel()
	log, err := os.Create("/tmp/mar66-tui.log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	go func() { _, _ = io.Copy(log, terminal) }()
	time.Sleep(8 * time.Second)
	_, _ = terminal.Write([]byte("\r"))
	time.Sleep(2 * time.Second)
	_, _ = terminal.Write([]byte("/plan\r"))
	time.Sleep(2 * time.Second)
	_, _ = terminal.Write([]byte("Use request_user_input to ask exactly one question: Should the test marker be BLUE or GREEN? Do not inspect files or execute commands. Wait for my answer.\r"))
	time.Sleep(time.Second)
	_, _ = terminal.Write([]byte("\r"))
	sent := false
	resumed := false
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				t.Fatal("observer closed")
			}
			t.Logf("state=%s pending=%+v", update.State, update.Pending)
			if !sent && update.State == bridge.InteractionWaitingForInput && update.Pending != nil {
				if err := p.RespondToInput(ctx, cfg.SessionID, update.Pending.ID, "BLUE"); err != nil {
					t.Fatal(err)
				}
				t.Log("response delivered through original provider request")
				if err := p.RespondToInput(ctx, cfg.SessionID, update.Pending.ID, "BLUE"); err == nil {
					t.Fatal("duplicate response accepted")
				}
				sent = true
			}
			if sent && update.State == bridge.InteractionWorking {
				if update.Pending != nil {
					t.Fatal("pending not cleared by provider")
				}
				resumed = true
			}
			if resumed && update.State == bridge.InteractionIdle {
				return
			}
		case <-ctx.Done():
			t.Fatal("real TUI did not produce an identified pending input request; inspect /tmp/mar66-tui.log")
		}
	}
}
