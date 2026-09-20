package bridgecontrol

import (
	"context"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

func TestSessionStatus_MapsEveryState(t *testing.T) {
	cases := map[bridge.SessionState]string{
		bridge.SessionStateStarting: StatusStarting,
		bridge.SessionStateRunning:  StatusRunning,
		bridge.SessionStateAttached: StatusAttached,
		bridge.SessionStateStopping: StatusStopping,
		bridge.SessionStateStopped:  StatusStopped,
		bridge.SessionStateFailed:   StatusFailed,
		bridge.SessionState(9999):   StatusUnknown,
	}
	for state, want := range cases {
		if got := sessionStatus(state); got != want {
			t.Fatalf("sessionStatus(%v) = %q, want %q", state, got, want)
		}
	}
}

func TestActiveSnapshots_ExcludesTerminalSessions(t *testing.T) {
	infos := []bridge.SessionInfo{
		{SessionID: "running", State: bridge.SessionStateRunning},
		{SessionID: "stopped", State: bridge.SessionStateStopped},
		{SessionID: "failed", State: bridge.SessionStateFailed},
		{SessionID: "starting", State: bridge.SessionStateStarting},
	}
	got := ActiveSnapshots(infos)
	if len(got) != 2 {
		t.Fatalf("ActiveSnapshots returned %d sessions, want 2: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.SessionID] = true
	}
	if !ids["running"] || !ids["starting"] {
		t.Fatalf("ActiveSnapshots missing expected sessions: %+v", got)
	}
}

func TestActiveSnapshots_EmptyInputProducesEmptyNotNilSlice(t *testing.T) {
	got := ActiveSnapshots(nil)
	if got == nil {
		t.Fatal("ActiveSnapshots(nil) returned nil, want a non-nil empty slice (must marshal to [] not null)")
	}
	if len(got) != 0 {
		t.Fatalf("ActiveSnapshots(nil) = %+v, want empty", got)
	}
}

func TestSupervisorObserver_ForwardsToClientNotify(t *testing.T) {
	c := New(Config{Endpoint: "ws://unused.invalid", Credential: "bri_x"})
	obs := NewSupervisorObserver(c)

	obs.SessionChanged(bridge.SessionInfo{SessionID: "s1", Provider: "codex", ProjectID: "p1", State: bridge.SessionStateRunning, CreatedAt: time.Now()})

	select {
	case got := <-c.events:
		if got.SessionID != "s1" || got.Status != StatusRunning {
			t.Fatalf("forwarded snapshot = %+v", got)
		}
	default:
		t.Fatal("expected SessionChanged to enqueue a notification")
	}
}

func TestSupervisorObserver_CloseDelegatesToClient(t *testing.T) {
	c := New(Config{Endpoint: "ws://unused.invalid", Credential: "bri_x"})
	obs := NewSupervisorObserver(c)
	c.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := obs.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
