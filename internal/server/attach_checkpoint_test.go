package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/auth"
	"github.com/orchael/bridgectl/internal/bridge"
)

// A local attach that cannot replay from the start receives the gap notice,
// then the current screen as output at last_seq, then only newer output.
func TestAttachSessionGapSendsScreenCheckpoint(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "cat", version: "1"}); err != nil {
		t.Fatal(err)
	}
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()
	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS: 1000, GlobalBurst: 1000,
		StartSessionPerClientRPS: 10, StartSessionPerClientBurst: 10,
		SendInputPerSessionRPS: 1000, SendInputPerSessionBurst: 1000,
	}, "test-instance", nil, nil, "", "")
	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "project-a"})
	sessionID := uuid.NewString()
	if _, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId: "project-a", SessionId: sessionID, RepoPath: t.TempDir(),
		Provider: "cat", InitialCols: 80, InitialRows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Attach(sessionID, "writer", 0, bridge.AttachRoleWriter); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		if _, err := supervisor.WriteInput(sessionID, "writer", []byte(strings.Repeat("x", 60)+"\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := supervisor.Get(sessionID)
		if err == nil && info.OldestSeq > 1 && info.LastSeq > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("output never left the replay window: %+v %v", info, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	stream := newAttachStream(ctx)
	done := make(chan error, 1)
	go func() {
		done <- s.AttachSession(&bridgev1.AttachSessionRequest{
			SessionId: sessionID,
			ClientId:  "observer",
			Role:      bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
		}, stream)
	}()
	waitForAttachEvent(t, stream, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP)
	time.Sleep(50 * time.Millisecond)
	stream.cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	events := stream.snapshot()
	gapAt := -1
	for i, ev := range events {
		if ev.GetType() == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP {
			gapAt = i
			break
		}
	}
	if gapAt < 0 || gapAt+1 >= len(events) {
		t.Fatalf("no checkpoint after gap: %v", events)
	}
	gap, cp := events[gapAt], events[gapAt+1]
	if cp.GetType() != bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT || cp.GetSeq() != gap.GetLastSeq() || !cp.GetReplay() {
		t.Fatalf("checkpoint event=%v gap=%v", cp, gap)
	}
	if !bytes.HasPrefix(cp.GetPayload(), []byte("\x1b[!p")) || !bytes.Contains(cp.GetPayload(), []byte("xxxxxxxx")) {
		t.Fatalf("checkpoint payload=%q", cp.GetPayload())
	}
	last := cp.GetSeq()
	for _, ev := range events[gapAt+2:] {
		if ev.GetType() == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT {
			if ev.GetSeq() != last+1 {
				t.Fatalf("output after checkpoint out of order: seq=%d after %d", ev.GetSeq(), last)
			}
			last = ev.GetSeq()
		}
	}
}
