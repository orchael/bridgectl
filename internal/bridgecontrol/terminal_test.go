package bridgecontrol

import (
	"context"
	"strings"
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

func TestTerminalSubscriptionDeliversWithoutClaimingWriterAndCancels(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(provider.NewStdioProvider(provider.StdioConfig{ProviderID: "cat", Binary: "/bin/cat"})); err != nil {
		t.Fatal(err)
	}
	s := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1<<20, time.Minute)
	defer s.Close()
	_, err := s.Start(context.Background(), bridge.SessionConfig{ProjectID: "test", SessionID: "stream", RepoPath: t.TempDir(), Options: map[string]string{"provider": "cat"}, InitialCols: 80, InitialRows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Attach("stream", "local", 0, bridge.AttachRoleWriter); err != nil {
		t.Fatal(err)
	}
	m := &terminalManager{supervisor: func() *bridge.Supervisor { return s }, ttl: time.Second}
	defer m.close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan TerminalResult, 16)
	m.startStream(ctx, TerminalRequest{ID: "stream-request", SessionID: "stream", ClientID: "browser", Action: "watch", Stream: true, ExpiresAt: time.Now().Add(8 * time.Second)}, func(r TerminalResult) error {
		select {
		case results <- r:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if _, err = s.WriteInput("stream", "local", []byte("STREAM_CHECK\n")); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "STREAM_CHECK") {
		select {
		case r := <-results:
			if r.Code != "" || r.Window.Writer {
				t.Fatal(r)
			}
			output.Write(r.Window.Data)
		case <-ctx.Done():
			t.Fatal("stream did not deliver output")
		}
	}
	m.cancelStream("stream-request")
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		n := len(m.streams)
		m.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel retained subscription")
		}
		time.Sleep(time.Millisecond)
	}
	info, err := s.Get("stream")
	if err != nil || info.ActiveWriterClientID != "local" {
		t.Fatal("subscription changed writer ownership")
	}
}
