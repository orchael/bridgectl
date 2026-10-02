package bridgecontrol

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orchael/bridgectl/internal/bridge"
)

func decodeSessionPayload(t *testing.T, env envelope) sessionPayload {
	t.Helper()
	var sp sessionPayload
	if err := json.Unmarshal(env.Payload, &sp); err != nil {
		t.Fatalf("decode sessionPayload: %v", err)
	}
	return sp
}

// TestSendSnapshot_AlwaysIncludesCurrentInteraction covers the ticket's
// requirement that a reconnect/authoritative snapshot always carries current
// interaction state, including full capability metadata and a pending
// request, on the very first snapshot ever sent for a session.
func TestSendSnapshot_AlwaysIncludesCurrentInteraction(t *testing.T) {
	snapshotSeen := make(chan envelope, 1)
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		_, snapEnv, err := serverHandshake(conn, 30)
		if err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		snapshotSeen <- snapEnv
		_, _ = serverReadEnvelope(conn)
	})
	defer fs.Close()

	now := time.Now()
	interaction := bridge.Interaction{
		State:          bridge.InteractionWaitingForApproval,
		Revision:       1,
		UpdatedAt:      now,
		LastActivityAt: now,
		Pending:        &bridge.PendingRequest{ID: "req-1", Type: bridge.PendingRequestApproval, Summary: "Apply migration?"},
		Evidence: bridge.InteractionEvidence{
			Source: "codex-app-server",
			Capability: bridge.InteractionCapabilities{
				InteractionStateSupported: true, ApprovalStateSupported: true, PendingSummarySupported: true,
			},
		},
	}
	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot {
		return []SessionSnapshot{{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusRunning, CreatedAt: now, Interaction: interaction}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() { _ = c.connectAndServe(ctx) }()

	var snapEnv envelope
	select {
	case snapEnv = <-snapshotSeen:
	case <-time.After(testTimeout):
		t.Fatal("snapshot never received")
	}
	var snap sessionSnapshotPayload
	if err := json.Unmarshal(snapEnv.Payload, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(snap.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(snap.Sessions))
	}
	got := snap.Sessions[0].Interaction
	if got == nil {
		t.Fatal("Interaction was omitted from the snapshot")
	}
	if got.State != InteractionWaitingForApproval {
		t.Fatalf("State = %q, want %q", got.State, InteractionWaitingForApproval)
	}
	if got.Revision != 1 {
		t.Fatalf("Revision = %d, want 1", got.Revision)
	}
	if got.Pending == nil || got.Pending.ID != "req-1" || got.Pending.Type != PendingRequestApproval || got.Pending.Summary != "Apply migration?" {
		t.Fatalf("Pending = %+v, want req-1/approval/summary", got.Pending)
	}
	if got.Source != "codex-app-server" {
		t.Fatalf("Source = %q, want codex-app-server", got.Source)
	}
	if !got.Capability.InteractionStateSupported || !got.Capability.ApprovalStateSupported || !got.Capability.PendingSummarySupported {
		t.Fatalf("Capability = %+v, want all true", got.Capability)
	}
}

// TestSendSnapshot_UnsupportedProviderReportsUnknown covers: a session whose
// provider never called UpdateInteraction (the default zero-value
// Interaction) is reported as an explicit, authoritative "unknown" — never
// silently omitted, and never a guess at some other state.
func TestSendSnapshot_UnsupportedProviderReportsUnknown(t *testing.T) {
	snapshotSeen := make(chan envelope, 1)
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		_, snapEnv, err := serverHandshake(conn, 30)
		if err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		snapshotSeen <- snapEnv
		_, _ = serverReadEnvelope(conn)
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot {
		return []SessionSnapshot{{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusRunning, CreatedAt: time.Now()}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() { _ = c.connectAndServe(ctx) }()

	var snapEnv envelope
	select {
	case snapEnv = <-snapshotSeen:
	case <-time.After(testTimeout):
		t.Fatal("snapshot never received")
	}
	var snap sessionSnapshotPayload
	if err := json.Unmarshal(snapEnv.Payload, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	got := snap.Sessions[0].Interaction
	if got == nil {
		t.Fatal("Interaction was omitted; an unsupported provider must still report an explicit unknown")
	}
	if got.State != InteractionUnknown {
		t.Fatalf("State = %q, want %q", got.State, InteractionUnknown)
	}
	if got.Capability.InteractionStateSupported || got.Capability.ApprovalStateSupported || got.Capability.PendingSummarySupported {
		t.Fatalf("Capability = %+v, want all false for an unsupported provider", got.Capability)
	}
	if got.Pending != nil {
		t.Fatalf("Pending = %+v, want nil", got.Pending)
	}
}

// TestSendEvent_InteractionOmittedWhenUnchanged covers: an incremental
// lifecycle event (session_updated) whose Interaction has not changed since
// the last thing this Client sent must omit the interaction field entirely,
// not resend a stale-looking duplicate that could confuse Bridge's "was this
// actually updated" bookkeeping.
func TestSendEvent_InteractionOmittedWhenUnchanged(t *testing.T) {
	var mu sync.Mutex
	var recorded []envelope
	snapshotSeen := make(chan struct{})
	var closeOnce sync.Once
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		closeOnce.Do(func() { close(snapshotSeen) })
		for {
			env, err := serverReadEnvelope(conn)
			if err != nil {
				return
			}
			mu.Lock()
			recorded = append(recorded, env)
			mu.Unlock()
			_ = serverWriteEnvelope(conn, msgAck, "srv-ack", ackPayload{MessageID: env.MessageID, Result: "applied"})
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() { _ = c.connectAndServe(ctx) }()
	select {
	case <-snapshotSeen:
	case <-time.After(testTimeout):
		t.Fatal("server never received the initial snapshot")
	}
	time.Sleep(50 * time.Millisecond)

	now := time.Now()
	interaction := bridge.Interaction{
		State: bridge.InteractionWorking, Revision: 1, UpdatedAt: now, LastActivityAt: now,
		Evidence: bridge.InteractionEvidence{Source: "test", Capability: bridge.InteractionCapabilities{InteractionStateSupported: true}},
	}
	// Event #1 (session_started): interaction must be included (first ever).
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusRunning, CreatedAt: now, Interaction: interaction})
	waitFor(t, testTimeout, func() bool { mu.Lock(); defer mu.Unlock(); return len(recorded) >= 1 })

	// Event #2 (session_updated): same Interaction.Revision (unchanged) —
	// only the lifecycle status differs (still "running" -> "attached").
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusAttached, CreatedAt: now, Interaction: interaction})
	waitFor(t, testTimeout, func() bool { mu.Lock(); defer mu.Unlock(); return len(recorded) >= 2 })

	mu.Lock()
	msgs := append([]envelope{}, recorded...)
	mu.Unlock()

	sp0 := decodeSessionPayload(t, msgs[0])
	if sp0.Interaction == nil {
		t.Fatal("first event: Interaction omitted, want included (first-ever report)")
	}
	firstWireRev := sp0.Interaction.Revision

	sp1 := decodeSessionPayload(t, msgs[1])
	if sp1.Interaction != nil {
		t.Fatalf("second event: Interaction = %+v, want omitted (unchanged since last report)", sp1.Interaction)
	}
	_ = firstWireRev
}

// TestSendEvent_InteractionIncludedWhenChanged is the mirror case: a genuine
// interaction change rides along with the next lifecycle event and advances
// the wire revision.
func TestSendEvent_InteractionIncludedWhenChanged(t *testing.T) {
	var mu sync.Mutex
	var recorded []envelope
	snapshotSeen := make(chan struct{})
	var closeOnce sync.Once
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		closeOnce.Do(func() { close(snapshotSeen) })
		for {
			env, err := serverReadEnvelope(conn)
			if err != nil {
				return
			}
			mu.Lock()
			recorded = append(recorded, env)
			mu.Unlock()
			_ = serverWriteEnvelope(conn, msgAck, "srv-ack", ackPayload{MessageID: env.MessageID, Result: "applied"})
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() { _ = c.connectAndServe(ctx) }()
	select {
	case <-snapshotSeen:
	case <-time.After(testTimeout):
		t.Fatal("server never received the initial snapshot")
	}
	time.Sleep(50 * time.Millisecond)

	now := time.Now()
	working := bridge.Interaction{State: bridge.InteractionWorking, Revision: 1, UpdatedAt: now, LastActivityAt: now,
		Evidence: bridge.InteractionEvidence{Source: "test"}}
	waiting := bridge.Interaction{
		State: bridge.InteractionWaitingForInput, Revision: 2, UpdatedAt: now, LastActivityAt: now,
		Pending:  &bridge.PendingRequest{ID: "req-1", Type: bridge.PendingRequestInput},
		Evidence: bridge.InteractionEvidence{Source: "test"},
	}
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusRunning, CreatedAt: now, Interaction: working})
	waitFor(t, testTimeout, func() bool { mu.Lock(); defer mu.Unlock(); return len(recorded) >= 1 })
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusRunning, CreatedAt: now, Interaction: waiting})
	waitFor(t, testTimeout, func() bool { mu.Lock(); defer mu.Unlock(); return len(recorded) >= 2 })

	mu.Lock()
	msgs := append([]envelope{}, recorded...)
	mu.Unlock()
	sp0 := decodeSessionPayload(t, msgs[0])
	sp1 := decodeSessionPayload(t, msgs[1])
	if sp0.Interaction == nil || sp1.Interaction == nil {
		t.Fatalf("both events must include Interaction: sp0=%v sp1=%v", sp0.Interaction, sp1.Interaction)
	}
	if sp1.Interaction.Revision <= sp0.Interaction.Revision {
		t.Fatalf("wire revision did not advance: %d -> %d", sp0.Interaction.Revision, sp1.Interaction.Revision)
	}
	if sp1.Interaction.State != InteractionWaitingForInput {
		t.Fatalf("State = %q, want %q", sp1.Interaction.State, InteractionWaitingForInput)
	}
	if sp1.Interaction.Pending == nil || sp1.Interaction.Pending.ID != "req-1" {
		t.Fatalf("Pending = %+v, want req-1", sp1.Interaction.Pending)
	}
}

// TestInteractionRevision_SurvivesRestart mirrors the exact restart-recovery
// reasoning RevisionStore's own doc comment gives for session lifecycle
// revisions: a second Client instance sharing the same RevisionPath (i.e.
// bridgectl daemon restarted, session recovered) must continue the
// interaction wire revision sequence rather than reissuing a low revision
// Bridge would reject as stale.
func TestInteractionRevision_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		BridgectlVersion: "test-version",
		InstallationID:   "install-1",
		StatusPath:       dir + "/status.json",
		RevisionPath:     dir + "/revisions.json",
		Logger:           testLogger(),
	}

	c1 := New(cfg)
	now := time.Now()
	interaction := bridge.Interaction{State: bridge.InteractionWorking, Revision: 1, UpdatedAt: now, LastActivityAt: now,
		Evidence: bridge.InteractionEvidence{Source: "test"}}
	p1 := c1.toInteractionPayload("s1", interaction, true)
	if p1 == nil || p1.Revision != 1 {
		t.Fatalf("first client's wire revision = %+v, want 1", p1)
	}

	// Simulate a daemon restart: a brand-new Client sharing the same
	// RevisionPath must not reissue revision 1 for a session it already
	// reported.
	c2 := New(cfg)
	interaction.Revision = 2 // a genuinely new local change after "restart"
	p2 := c2.toInteractionPayload("s1", interaction, true)
	if p2 == nil || p2.Revision != 2 {
		t.Fatalf("second client's wire revision = %+v, want 2 (continuing from disk)", p2)
	}
}

// TestInteractionAndLifecycleRevisions_AreIndependent covers the product
// model's central invariant: interaction revision and session lifecycle
// revision are separate counters that must never influence each other.
func TestInteractionAndLifecycleRevisions_AreIndependent(t *testing.T) {
	c := newTestClient(t, "ws://unused", "bri_valid", func() []SessionSnapshot { return nil })
	// Advance the lifecycle revision several times without ever touching
	// interaction.
	_ = c.revisions.Next("s1")
	_ = c.revisions.Next("s1")
	_ = c.revisions.Next("s1")
	if got := c.revisions.Current("s1"); got != 3 {
		t.Fatalf("lifecycle revision = %d, want 3", got)
	}
	if got := c.interactions.Current("s1"); got != 0 {
		t.Fatalf("interaction revision = %d, want untouched at 0", got)
	}
	now := time.Now()
	p := c.toInteractionPayload("s1", bridge.Interaction{State: bridge.InteractionWorking, Revision: 1, UpdatedAt: now, LastActivityAt: now, Evidence: bridge.InteractionEvidence{Source: "t"}}, true)
	if p == nil || p.Revision != 1 {
		t.Fatalf("interaction wire revision = %+v, want to start at 1 regardless of lifecycle revision already at 3", p)
	}
}
