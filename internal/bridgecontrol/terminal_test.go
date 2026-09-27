package bridgecontrol

import (
	"context"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/provider"
)

func TestTerminalWatchAttachInputReleaseAndExpiry(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(provider.NewStdioProvider(provider.StdioConfig{ProviderID: "cat", Binary: "/bin/cat"})); err != nil {
		t.Fatal(err)
	}
	s := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1<<20, time.Minute)
	defer s.Close()
	_, err := s.Start(context.Background(), bridge.SessionConfig{ProjectID: "test", SessionID: "terminal", RepoPath: t.TempDir(), Options: map[string]string{"provider": "cat"}, InitialCols: 80, InitialRows: 24})
	if err != nil {
		t.Fatal(err)
	}
	m := &terminalManager{supervisor: func() *bridge.Supervisor { return s }, ttl: 100 * time.Millisecond}
	defer m.close()
	r := TerminalRequest{ID: "request", SessionID: "terminal", ClientID: "browser", Action: "watch", ExpiresAt: time.Now().Add(10 * time.Second)}
	call := func(action string) TerminalResult { t.Helper(); r.Action = action; return m.handle(r) }
	if got := call("watch"); got.Code != "" || got.Window.Writer {
		t.Fatal(got)
	}
	r.Data = []byte("WATCH_MUST_NOT_WRITE\n")
	if got := call("input"); got.Code != "not_attached" {
		t.Fatal(got)
	}
	r.Data = nil
	if _, err := s.Attach("terminal", "local", 0, bridge.AttachRoleWriter); err != nil {
		t.Fatal(err)
	}
	if got := call("attach"); got.Code != "writer_conflict" {
		t.Fatal(got)
	}
	_, _ = s.Detach("terminal", "local")
	if got := call("attach"); got.Code != "" || !got.Window.Writer {
		t.Fatal(got)
	}
	r.Cols = 100
	r.Rows = 30
	if got := call("resize"); got.Code != "" || got.Window.Cols != 100 {
		t.Fatal(got)
	}
	r.Data = []byte("TERMINAL_INPUT\n")
	if got := call("input"); got.Code != "" {
		t.Fatal(got)
	}
	r.Data = nil
	deadline := time.Now().Add(time.Second)
	r.KeepWriter = true
	for {
		w := call("watch")
		if len(w.Window.Data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PTY output not received")
		}
		time.Sleep(time.Millisecond)
	}
	if got := call("detach"); got.Code != "" || got.Window.Writer {
		t.Fatal(got)
	}
	r.KeepWriter = false
	if got := call("input"); got.Code != "not_attached" {
		t.Fatal(got)
	}
	if got := call("attach"); got.Code != "" {
		t.Fatal(got)
	}
	deadline = time.Now().Add(time.Second)
	for {
		info, _ := s.Get("terminal")
		if info.ActiveWriterClientID == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer lease did not expire")
		}
		time.Sleep(time.Millisecond)
	}
	if got := call("watch"); got.Window.Writer {
		t.Fatal("watch reacquired expired writer")
	}
	r.ExpiresAt = time.Now().Add(-time.Second)
	if got := call("attach"); got.Code != "invalid_request" {
		t.Fatal(got)
	}
}
