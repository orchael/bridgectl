package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

// interactionAwareTestProvider is a testProvider that also declares
// interaction capabilities, so tests can exercise the capability-gated path
// without depending on any real provider's process behavior.
type interactionAwareTestProvider struct {
	testProvider
	caps InteractionCapabilities
}

func (p *interactionAwareTestProvider) InteractionCapabilities() InteractionCapabilities {
	return p.caps
}

func newInteractionSupervisor(t *testing.T, caps *InteractionCapabilities) (*Supervisor, *controlSpy) {
	t.Helper()
	registry := NewRegistry()
	var prov Provider = &testProvider{id: "fake"}
	if caps != nil {
		prov = &interactionAwareTestProvider{testProvider: testProvider{id: "fake"}, caps: *caps}
	}
	if err := registry.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}
	spy := newControlSpy()
	sup := NewSupervisor(registry, DefaultPolicy(), 1024*1024, time.Minute, WithControlObserver(spy))
	t.Cleanup(func() { sup.Close() })
	return sup, spy
}

// TestSession_DefaultInteractionIsUnknown covers: a provider that does not
// implement InteractionCapableProvider at all must produce a session whose
// Interaction starts at Unknown with an all-false capability, never a guess.
func TestSession_DefaultInteractionIsUnknown(t *testing.T) {
	sup, _ := newInteractionSupervisor(t, nil)
	info := startTestSession(t, sup, "s1")
	if info.Interaction.EffectiveState() != InteractionUnknown {
		t.Fatalf("Interaction.State = %q, want Unknown", info.Interaction.EffectiveState())
	}
	if info.InteractionCapabilities != (InteractionCapabilities{}) {
		t.Fatalf("InteractionCapabilities = %+v, want zero value", info.InteractionCapabilities)
	}
	if info.Interaction.Pending != nil {
		t.Fatalf("Pending = %+v, want nil", info.Interaction.Pending)
	}
}

// TestSession_CapabilitiesReportedFromProvider covers: provider capability
// reporting is exposed onto SessionInfo verbatim.
func TestSession_CapabilitiesReportedFromProvider(t *testing.T) {
	caps := InteractionCapabilities{InteractionStateSupported: true, ApprovalStateSupported: true, PendingSummarySupported: true}
	sup, _ := newInteractionSupervisor(t, &caps)
	info := startTestSession(t, sup, "s1")
	if info.InteractionCapabilities != caps {
		t.Fatalf("InteractionCapabilities = %+v, want %+v", info.InteractionCapabilities, caps)
	}
	// Unknown until an authoritative signal actually arrives: capability
	// alone must never be treated as evidence of a particular state.
	if info.Interaction.EffectiveState() != InteractionUnknown {
		t.Fatalf("Interaction.State = %q, want Unknown before any authoritative report", info.Interaction.EffectiveState())
	}
}

// TestUpdateInteraction_RequiresEvidenceSource covers: a call with no
// evidence source is rejected outright, since it can't be authoritative by
// definition.
func TestUpdateInteraction_RequiresEvidenceSource(t *testing.T) {
	sup, _ := newInteractionSupervisor(t, nil)
	startTestSession(t, sup, "s1")
	err := sup.UpdateInteraction("s1", Interaction{State: InteractionWorking})
	if err == nil {
		t.Fatal("UpdateInteraction with empty Evidence.Source: want error, got nil")
	}
}

// TestUpdateInteraction_UnknownSession covers a not-found session.
func TestUpdateInteraction_UnknownSession(t *testing.T) {
	sup, _ := newInteractionSupervisor(t, nil)
	err := sup.UpdateInteraction("does-not-exist", Interaction{
		State:    InteractionWorking,
		Evidence: InteractionEvidence{Source: "test"},
	})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

// TestUpdateInteraction_MonotonicRevision covers: revision only advances on
// a real change (state or pending identity), never on a repeated report of
// the same value, and never goes backwards.
func TestUpdateInteraction_MonotonicRevision(t *testing.T) {
	sup, spy := newInteractionSupervisor(t, nil)
	startTestSession(t, sup, "s1")
	evidence := InteractionEvidence{Source: "test-signal"}

	if err := sup.UpdateInteraction("s1", Interaction{State: InteractionWorking, Evidence: evidence}); err != nil {
		t.Fatalf("UpdateInteraction #1: %v", err)
	}
	info, err := sup.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Interaction.Revision != 1 {
		t.Fatalf("revision after first change = %d, want 1", info.Interaction.Revision)
	}
	firstUpdatedAt := info.Interaction.UpdatedAt

	// A repeated report of the identical state must not bump the revision
	// or UpdatedAt, only LastActivityAt.
	time.Sleep(2 * time.Millisecond)
	if err := sup.UpdateInteraction("s1", Interaction{State: InteractionWorking, Evidence: evidence}); err != nil {
		t.Fatalf("UpdateInteraction #2 (no-op): %v", err)
	}
	info, err = sup.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Interaction.Revision != 1 {
		t.Fatalf("revision after repeated report = %d, want unchanged 1", info.Interaction.Revision)
	}
	if !info.Interaction.UpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("UpdatedAt changed on a no-op report: got %v, want unchanged %v", info.Interaction.UpdatedAt, firstUpdatedAt)
	}
	if !info.Interaction.LastActivityAt.After(firstUpdatedAt) {
		t.Fatalf("LastActivityAt did not advance on a no-op report")
	}

	// A genuine transition bumps the revision again.
	if err := sup.UpdateInteraction("s1", Interaction{State: InteractionIdle, Evidence: evidence}); err != nil {
		t.Fatalf("UpdateInteraction #3: %v", err)
	}
	info, err = sup.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Interaction.Revision != 2 {
		t.Fatalf("revision after second change = %d, want 2", info.Interaction.Revision)
	}

	// Every call (including the no-op) must have reached the control
	// observer, since a heartbeat-like refresh is still useful downstream
	// for staleness reasoning even without a state transition.
	changes := spy.states()
	_ = changes // lifecycle states only; interaction changes checked below.
	spy.mu.Lock()
	interactionCalls := 0
	for _, c := range spy.changes {
		if c.SessionID == "s1" {
			interactionCalls++
		}
	}
	spy.mu.Unlock()
	if interactionCalls < 3 {
		t.Fatalf("control observer saw %d SessionChanged calls for s1, want at least 3", interactionCalls)
	}
}

// TestUpdateInteraction_PendingRequestStableIdentity covers the ticket's
// core "stable pending request identity" requirement: the same logical
// request must keep the same revision/identity across a repeated report,
// and a *different* pending request (even with the same Type) must bump
// the revision as a real change.
func TestUpdateInteraction_PendingRequestStableIdentity(t *testing.T) {
	sup, _ := newInteractionSupervisor(t, nil)
	startTestSession(t, sup, "s1")
	evidence := InteractionEvidence{Source: "test-signal"}
	req := &PendingRequest{ID: "req-1", Type: PendingRequestApproval, Summary: "Apply migration?"}

	if err := sup.UpdateInteraction("s1", Interaction{
		State: InteractionWaitingForApproval, Pending: req, Evidence: evidence,
	}); err != nil {
		t.Fatal(err)
	}
	info, _ := sup.Get("s1")
	if info.Interaction.Pending == nil || info.Interaction.Pending.ID != "req-1" {
		t.Fatalf("Pending = %+v, want req-1", info.Interaction.Pending)
	}
	rev1 := info.Interaction.Revision

	// Re-reporting the exact same pending request (same ID/Type/Summary)
	// must not bump the revision.
	if err := sup.UpdateInteraction("s1", Interaction{
		State:    InteractionWaitingForApproval,
		Pending:  &PendingRequest{ID: "req-1", Type: PendingRequestApproval, Summary: "Apply migration?"},
		Evidence: evidence,
	}); err != nil {
		t.Fatal(err)
	}
	info, _ = sup.Get("s1")
	if info.Interaction.Revision != rev1 {
		t.Fatalf("revision bumped on identical pending request: got %d, want %d", info.Interaction.Revision, rev1)
	}

	// A new pending request (different ID) is a real change even though
	// State and Type are unchanged.
	if err := sup.UpdateInteraction("s1", Interaction{
		State:    InteractionWaitingForApproval,
		Pending:  &PendingRequest{ID: "req-2", Type: PendingRequestApproval, Summary: "Delete branch?"},
		Evidence: evidence,
	}); err != nil {
		t.Fatal(err)
	}
	info, _ = sup.Get("s1")
	if info.Interaction.Revision != rev1+1 {
		t.Fatalf("revision after new pending request = %d, want %d", info.Interaction.Revision, rev1+1)
	}
	if info.Interaction.Pending.ID != "req-2" {
		t.Fatalf("Pending.ID = %q, want req-2", info.Interaction.Pending.ID)
	}
}

// TestWriteInput_DoesNotClearPendingRequest is the ticket's central
// regression guard: bridgectl successfully writing bytes to a session's
// stdin must never, by itself, clear a pending interaction request. Only a
// fresh authoritative UpdateInteraction call may do that.
func TestWriteInput_DoesNotClearPendingRequest(t *testing.T) {
	sup, _ := newInteractionSupervisor(t, nil)
	startTestSession(t, sup, "s1")
	req := &PendingRequest{ID: "req-1", Type: PendingRequestInput}
	if err := sup.UpdateInteraction("s1", Interaction{
		State: InteractionWaitingForInput, Pending: req,
		Evidence: InteractionEvidence{Source: "test-signal"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := sup.Attach("s1", "writer-1", 0, AttachRoleWriter); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := sup.WriteInput("s1", "writer-1", []byte("yes please\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	info, err := sup.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Interaction.EffectiveState() != InteractionWaitingForInput {
		t.Fatalf("Interaction.State after WriteInput = %q, want still waiting_for_input", info.Interaction.EffectiveState())
	}
	if info.Interaction.Pending == nil || info.Interaction.Pending.ID != "req-1" {
		t.Fatalf("Pending after WriteInput = %+v, want still req-1", info.Interaction.Pending)
	}
}

// TestUpdateInteraction_AuthoritativeTransitionClearsPending covers the
// flip side: real provider evidence of a resolved/superseded request does
// clear it.
func TestUpdateInteraction_AuthoritativeTransitionClearsPending(t *testing.T) {
	sup, _ := newInteractionSupervisor(t, nil)
	startTestSession(t, sup, "s1")
	evidence := InteractionEvidence{Source: "test-signal"}
	if err := sup.UpdateInteraction("s1", Interaction{
		State: InteractionWaitingForInput, Pending: &PendingRequest{ID: "req-1", Type: PendingRequestInput}, Evidence: evidence,
	}); err != nil {
		t.Fatal(err)
	}
	// The provider authoritatively reports the turn resumed.
	if err := sup.UpdateInteraction("s1", Interaction{State: InteractionWorking, Evidence: evidence}); err != nil {
		t.Fatal(err)
	}
	info, _ := sup.Get("s1")
	if info.Interaction.EffectiveState() != InteractionWorking {
		t.Fatalf("Interaction.State = %q, want working", info.Interaction.EffectiveState())
	}
	if info.Interaction.Pending != nil {
		t.Fatalf("Pending = %+v, want nil after authoritative resolution", info.Interaction.Pending)
	}
}

// TestClaudeStreamJSON_AuthoritativeWorkingIdle exercises the one provider
// wired to a genuine structural signal end-to-end: the Anthropic Messages
// streaming protocol's own message_start/message_stop events, consumed by
// readLoopStreamJSON, must flip Interaction between Working and Idle without
// any output-content heuristics.
func TestClaudeStreamJSON_AuthoritativeWorkingIdle(t *testing.T) {
	registry := NewRegistry()
	prov := &interactionCapableStreamJSONProvider{
		streamJSONTestProvider: streamJSONTestProvider{
			testProvider: testProvider{id: "claude-chat-fake"},
			jsonLines: []string{
				`{"type":"message_start"}`,
				`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
				`{"type":"message_stop"}`,
			},
		},
		caps: InteractionCapabilities{InteractionStateSupported: true},
	}
	if err := registry.Register(prov); err != nil {
		t.Fatal(err)
	}
	spy := newControlSpy()
	sup := NewSupervisor(registry, DefaultPolicy(), 1024*1024, time.Minute, WithControlObserver(spy))
	t.Cleanup(func() { sup.Close() })

	info, err := sup.Start(context.Background(), SessionConfig{
		ProjectID: "p", SessionID: "stream-1", RepoPath: t.TempDir(),
		Options: map[string]string{"provider": "claude-chat-fake"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Interaction.EffectiveState() != InteractionUnknown {
		t.Fatalf("initial state = %q, want unknown before any event", info.Interaction.EffectiveState())
	}

	// The scripted process runs and exits in well under a millisecond, so
	// polling current state can race straight past the transient Working
	// state to the final Idle one. Assert on the ControlObserver's full
	// history instead, which captures every authoritative transition in
	// order regardless of how fast they happened.
	waitForInteraction(t, sup, "stream-1", InteractionIdle)

	spy.mu.Lock()
	var seen []InteractionStateValue
	for _, c := range spy.changes {
		if c.SessionID == "stream-1" {
			seen = append(seen, c.Interaction.EffectiveState())
		}
	}
	spy.mu.Unlock()
	foundWorking, foundIdleAfterWorking := false, false
	for _, s := range seen {
		if s == InteractionWorking {
			foundWorking = true
		}
		if s == InteractionIdle && foundWorking {
			foundIdleAfterWorking = true
		}
	}
	if !foundWorking || !foundIdleAfterWorking {
		t.Fatalf("control observer interaction history = %v, want Working before Idle", seen)
	}
}

func waitForInteraction(t *testing.T, sup *Supervisor, sessionID string, want InteractionStateValue) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, err := sup.Get(sessionID)
		if err == nil && info.Interaction.EffectiveState() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s never reached interaction state %q", sessionID, want)
}

// interactionCapableStreamJSONProvider wraps the existing
// streamJSONTestProvider fixture (supervisor_test.go) with a declared
// InteractionCapabilities, exercising
// Supervisor.observeStreamJSONInteraction exactly as the real claude-chat
// provider would against its scripted message_start/message_stop sequence.
type interactionCapableStreamJSONProvider struct {
	streamJSONTestProvider
	caps InteractionCapabilities
}

func (p *interactionCapableStreamJSONProvider) InteractionCapabilities() InteractionCapabilities {
	return p.caps
}
