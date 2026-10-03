package server

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/auth"
	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/diagnose"
)

// diagProvider is a real Provider whose process is `cat` (stays running) or
// `false` (fails at once), optionally declaring interaction capabilities.
type diagProvider struct {
	serverTestProvider
	caps *bridge.InteractionCapabilities
	cmd  string
}

func (p *diagProvider) BuildCommand(context.Context, bridge.SessionConfig) (*exec.Cmd, error) {
	return exec.Command(p.cmd), nil
}

type diagCapable struct{ diagProvider }

func (p *diagCapable) InteractionCapabilities() bridge.InteractionCapabilities { return *p.caps }

func newDiagServer(t *testing.T) (*BridgeServer, *bridge.Supervisor) {
	t.Helper()
	falseBin, err := exec.LookPath("false")
	if err != nil {
		t.Skip("false binary unavailable")
	}
	reg := bridge.NewRegistry()
	caps := bridge.InteractionCapabilities{InteractionStateSupported: true, ApprovalStateSupported: true, PendingSummarySupported: true}
	for _, p := range []bridge.Provider{
		&diagProvider{serverTestProvider: serverTestProvider{id: "plain"}, cmd: "/bin/cat"},
		&diagCapable{diagProvider{serverTestProvider: serverTestProvider{id: "capable"}, cmd: "/bin/cat", caps: &caps}},
		&diagProvider{serverTestProvider: serverTestProvider{id: "failing"}, cmd: falseBin},
	} {
		if err := reg.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	sup := bridge.NewSupervisor(reg, bridge.DefaultPolicy(), 1024*1024, time.Minute)
	t.Cleanup(sup.Close)
	return New(sup, reg, slog.Default(), RateLimitConfig{}, "test", nil, nil, "", ""), sup
}

func diagStart(t *testing.T, s *BridgeServer, id, prov string) {
	t.Helper()
	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})
	if _, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{ProjectId: "proj", SessionId: id, RepoPath: t.TempDir(), Provider: prov}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
}

// diagnoseViaPublicAPI is exactly what the CLI does: GetSession RPC → Build.
func diagnoseViaPublicAPI(t *testing.T, s *BridgeServer, id string) (*diagnose.Report, *bridgev1.GetSessionResponse) {
	t.Helper()
	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})
	resp, err := s.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: id})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return diagnose.Build(resp, diagnose.Inputs{Version: "test", Now: time.Now()}), resp
}

func statusName(s bridgev1.SessionStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "SESSION_STATUS_"))
}

// assertAgrees checks every shared field of the diagnostic against the
// public GetSession response and the Supervisor's own SessionInfo.
func assertAgrees(t *testing.T, sup *bridge.Supervisor, rep *diagnose.Report, resp *bridgev1.GetSessionResponse) {
	t.Helper()
	info, err := sup.Get(resp.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SessionID != resp.SessionId || rep.Provider != resp.Provider || rep.ProjectID != resp.ProjectId {
		t.Errorf("identity mismatch: %+v vs %+v", rep, resp)
	}
	if rep.Status != statusName(resp.Status) {
		t.Errorf("status = %q, GetSession = %q", rep.Status, statusName(resp.Status))
	}
	if want := map[bridge.SessionState]string{
		bridge.SessionStateStarting: "starting", bridge.SessionStateRunning: "running", bridge.SessionStateAttached: "attached",
		bridge.SessionStateStopping: "stopping", bridge.SessionStateStopped: "stopped", bridge.SessionStateFailed: "failed",
	}[info.State]; rep.Status != want {
		t.Errorf("status = %q, Supervisor state = %q", rep.Status, want)
	}
	if string(info.Interaction.EffectiveState()) != rep.InteractionState {
		t.Errorf("interaction_state = %q, Supervisor = %q", rep.InteractionState, info.Interaction.EffectiveState())
	}
	if rep.InteractionRevisionLocal == nil || *rep.InteractionRevisionLocal != info.Interaction.Revision {
		t.Errorf("interaction_revision_local = %v, Supervisor = %d", rep.InteractionRevisionLocal, info.Interaction.Revision)
	}
	c := info.InteractionCapabilities
	if rep.InteractionCapability == nil || *rep.InteractionCapability != (diagnose.Capability{
		InteractionStateSupported: c.InteractionStateSupported, ApprovalStateSupported: c.ApprovalStateSupported,
		PendingSummarySupported: c.PendingSummarySupported, RemoteResponseSupported: c.RemoteResponseSupported,
		StructuredApprovalSupported: c.StructuredApprovalSupported,
	}) {
		t.Errorf("capability = %+v, Supervisor = %+v", rep.InteractionCapability, c)
	}
	if (info.Interaction.Pending == nil) != (rep.PendingRequest == nil) {
		t.Fatalf("pending presence differs: %+v vs %+v", rep.PendingRequest, info.Interaction.Pending)
	}
	if p := info.Interaction.Pending; p != nil {
		if rep.PendingRequest.ID != p.ID || rep.PendingRequest.Type != string(p.Type) {
			t.Errorf("pending = %+v, Supervisor = %+v", rep.PendingRequest, p)
		}
	}
	if rep.ActiveWriter != (info.ActiveWriterClientID != "") || rep.ObserverCount != info.ObserverCount {
		t.Errorf("writer/observers = %v/%d, Supervisor = %q/%d", rep.ActiveWriter, rep.ObserverCount, info.ActiveWriterClientID, info.ObserverCount)
	}
	if rep.ExitCode != nil != info.ExitRecorded || (rep.ExitCode != nil && int(*rep.ExitCode) != info.ExitCode) {
		t.Errorf("exit_code = %v, Supervisor recorded=%v code=%d", rep.ExitCode, info.ExitRecorded, info.ExitCode)
	}
}

func waitDiag(t *testing.T, sup *bridge.Supervisor, id string, want bridge.SessionState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := sup.Get(id); err == nil && info.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s never reached state %v", id, want)
}

func TestDiagnoseAgreesWithSupervisor_ProviderWithoutInteraction(t *testing.T) {
	s, sup := newDiagServer(t)
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	diagStart(t, s, id, "plain")

	rep, resp := diagnoseViaPublicAPI(t, s, id)
	assertAgrees(t, sup, rep, resp)
	if rep.InteractionState != "unknown" {
		t.Fatalf("provider with no interaction capability must report unknown, got %q", rep.InteractionState)
	}
	if rep.InteractionCapability == nil || *rep.InteractionCapability != (diagnose.Capability{}) {
		t.Fatalf("capability must be explicitly all-false (unsupported), got %+v", rep.InteractionCapability)
	}
	if rep.ActiveWriter {
		t.Fatal("no writer attached yet")
	}
}

func TestDiagnoseAgreesWithSupervisor_InteractionTransitions(t *testing.T) {
	s, sup := newDiagServer(t)
	const id = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	diagStart(t, s, id, "capable")
	caps := bridge.InteractionCapabilities{InteractionStateSupported: true, ApprovalStateSupported: true, PendingSummarySupported: true}
	ev := bridge.InteractionEvidence{Source: "test-provider", Capability: caps}

	steps := []struct {
		name string
		in   bridge.Interaction
	}{
		{"working", bridge.Interaction{State: bridge.InteractionWorking, Evidence: ev}},
		{"waiting_for_input", bridge.Interaction{State: bridge.InteractionWaitingForInput, Evidence: ev,
			Pending: &bridge.PendingRequest{ID: "q-1", Type: bridge.PendingRequestInput, Summary: "Which file?"}}},
		{"waiting_for_approval", bridge.Interaction{State: bridge.InteractionWaitingForApproval, Evidence: ev,
			Pending: &bridge.PendingRequest{ID: "a-1", Type: bridge.PendingRequestApproval, Summary: "Run tests"}}},
		{"working again", bridge.Interaction{State: bridge.InteractionWorking, Evidence: ev}},
		{"idle", bridge.Interaction{State: bridge.InteractionIdle, Evidence: ev}},
	}
	var lastRev uint64
	for _, st := range steps {
		if err := sup.UpdateInteraction(id, st.in); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		rep, resp := diagnoseViaPublicAPI(t, s, id)
		assertAgrees(t, sup, rep, resp)
		if rep.InteractionState != string(st.in.State) {
			t.Errorf("%s: interaction_state = %q", st.name, rep.InteractionState)
		}
		if *rep.InteractionRevisionLocal <= lastRev {
			t.Errorf("%s: interaction revision did not advance (%d -> %d)", st.name, lastRev, *rep.InteractionRevisionLocal)
		}
		lastRev = *rep.InteractionRevisionLocal
		if st.in.Pending != nil {
			if rep.PendingRequest.Summary == nil || *rep.PendingRequest.Summary != st.in.Pending.Summary {
				t.Errorf("%s: summary = %v", st.name, rep.PendingRequest.Summary)
			}
		}
	}

	// A repeated identical report refreshes last-report time only: same revision.
	if err := sup.UpdateInteraction(id, steps[len(steps)-1].in); err != nil {
		t.Fatal(err)
	}
	rep, _ := diagnoseViaPublicAPI(t, s, id)
	if *rep.InteractionRevisionLocal != lastRev {
		t.Errorf("repeat report changed revision: %d -> %d", lastRev, *rep.InteractionRevisionLocal)
	}
}

func TestDiagnoseAgreesWithSupervisor_WriterAndLifecycle(t *testing.T) {
	s, sup := newDiagServer(t)
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	diagStart(t, s, id, "plain")

	if _, err := sup.Attach(id, "writer-client-xyz", 0, bridge.AttachRoleWriter); err != nil {
		t.Fatal(err)
	}
	rep, resp := diagnoseViaPublicAPI(t, s, id)
	assertAgrees(t, sup, rep, resp)
	if !rep.ActiveWriter {
		t.Fatal("active writer must be reported after a writer attaches")
	}
	if b, _ := rep.MarshalJSON(); strings.Contains(string(b), "writer-client-xyz") {
		t.Fatal("writer client ID leaked")
	}

	if err := sup.Stop(id, true); err != nil {
		t.Fatal(err)
	}
	waitDiag(t, sup, id, bridge.SessionStateStopped)
	rep, resp = diagnoseViaPublicAPI(t, s, id)
	assertAgrees(t, sup, rep, resp)
	if rep.Status != "stopped" || rep.ExitCode == nil || rep.StoppedAt == nil {
		t.Fatalf("stopped session report = %+v", rep)
	}
	if rep.InteractionState != "unknown" {
		t.Fatalf("a stopped session must not gain an inferred interaction state: %q", rep.InteractionState)
	}
}

// TestDiagnoseAgreesWithSupervisor_NonZeroExit covers a process that exits
// non-zero on its own. Which terminal state the Supervisor assigns (the PTY
// read error can win the race and mark it stopped rather than failed) is the
// Supervisor's decision; the diagnostic must simply agree with it. The
// "failed" status mapping itself is covered in internal/diagnose.
func TestDiagnoseAgreesWithSupervisor_NonZeroExit(t *testing.T) {
	s, sup := newDiagServer(t)
	const id = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	diagStart(t, s, id, "failing")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := sup.Get(id); err == nil && info.ExitRecorded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rep, resp := diagnoseViaPublicAPI(t, s, id)
	assertAgrees(t, sup, rep, resp)
	if rep.ExitCode == nil || *rep.ExitCode == 0 {
		t.Fatalf("expected recorded non-zero exit, got %+v", rep)
	}
	if rep.Status != "failed" && rep.Status != "stopped" {
		t.Fatalf("status = %q, want a terminal state", rep.Status)
	}
	// SessionInfo.Error carries the process error text; it must not surface.
	if b, _ := rep.MarshalJSON(); strings.Contains(string(b), "exit status") {
		t.Fatalf("SessionInfo.Error leaked into diagnostics: %s", b)
	}
}

func TestDiagnoseUnknownSessionIsError(t *testing.T) {
	s, _ := newDiagServer(t)
	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})
	if _, err := s.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"}); err == nil {
		t.Fatal("GetSession for unknown session must fail so diagnose reports an error, not an empty snapshot")
	}
}
