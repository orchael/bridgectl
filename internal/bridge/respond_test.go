package bridge

import (
	"context"
	"errors"
	"testing"
)

type responseTestProvider struct {
	testProvider
	calls int
}

func (p *responseTestProvider) RespondToInput(context.Context, string, string, string) error {
	p.calls++
	return nil
}

func TestMAR66RespondValidationAndAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*managedSession)
		request string
		want    error
	}{
		{name: "accepted", request: "pending"},
		{name: "stale", request: "old", want: ErrPendingRequestMismatch},
		{name: "resolved", request: "pending", mutate: func(m *managedSession) { m.info.Interaction.Pending = nil }, want: ErrPendingRequestMismatch},
		{name: "stopped", request: "pending", mutate: func(m *managedSession) { m.info.State = SessionStateStopped }, want: ErrSessionNotRunning},
		{name: "local writer", request: "pending", mutate: func(m *managedSession) { m.info.ActiveWriterClientID = "local" }, want: ErrWriterConflict},
		{name: "approval", request: "pending", mutate: func(m *managedSession) { m.info.Interaction.State = InteractionWaitingForApproval }, want: ErrPendingRequestMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &responseTestProvider{}
			m := &managedSession{provider: p, info: SessionInfo{State: SessionStateRunning, Interaction: Interaction{State: InteractionWaitingForInput, Pending: &PendingRequest{ID: "pending", Type: PendingRequestInput}}}}
			if tc.mutate != nil {
				tc.mutate(m)
			}
			s := &Supervisor{sessions: map[string]*managedSession{"s": m}}
			err := s.RespondToInput(context.Background(), "s", tc.request, "answer")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if tc.want != nil && p.calls != 0 {
				t.Fatal("rejected response reached provider")
			}
			if tc.want == nil && (p.calls != 1 || m.info.Interaction.Pending == nil || m.info.Interaction.State != InteractionWaitingForInput) {
				t.Fatal("delivery must not clear attention")
			}
		})
	}
}

func (p *responseTestProvider) DecideApproval(context.Context, string, string, string) error {
	p.calls++
	return nil
}
func TestMAR66ApprovalOwnership(t *testing.T) {
	for _, kind := range []string{"accepted", "writer", "stale", "stopped", "unsupported", "input", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			p := &responseTestProvider{}
			m := &managedSession{provider: p, info: SessionInfo{State: SessionStateRunning, Interaction: Interaction{State: InteractionWaitingForApproval, Pending: &PendingRequest{ID: "rpc", Type: PendingRequestApproval}, Evidence: InteractionEvidence{Capability: InteractionCapabilities{StructuredApprovalSupported: true}}}}}
			id, decision := "rpc", "accept"
			switch kind {
			case "writer":
				m.info.ActiveWriterClientID = "local"
			case "stale":
				id = "old"
			case "stopped":
				m.info.State = SessionStateStopped
			case "unsupported":
				m.info.Interaction.Evidence.Capability.StructuredApprovalSupported = false
			case "input":
				m.info.Interaction.State = InteractionWaitingForInput
			case "invalid":
				decision = "acceptForSession"
			}
			s := &Supervisor{sessions: map[string]*managedSession{"s": m}}
			err := s.DecideApproval(context.Background(), "s", id, decision)
			if kind == "accepted" {
				if err != nil || p.calls != 1 || m.info.Interaction.Pending == nil {
					t.Fatal("approval must preserve pending", err)
				}
			} else if err == nil || p.calls != 0 {
				t.Fatal("unsafe approval dispatched")
			}
		})
	}
}

func (p *responseTestProvider) SendInstruction(context.Context, string, string) error {
	p.calls++
	return nil
}
func TestMAR95InstructionWriterOwnership(t *testing.T) {
	for _, writer := range []string{"", "local"} {
		p := &responseTestProvider{}
		m := &managedSession{provider: p, info: SessionInfo{State: SessionStateRunning, ActiveWriterClientID: writer}}
		s := &Supervisor{sessions: map[string]*managedSession{"s": m}}
		err := s.SendInstruction(context.Background(), "s", "Run tests")
		if writer != "" {
			if !errors.Is(err, ErrWriterConflict) || p.calls != 0 {
				t.Fatal("writer stolen", err)
			}
		} else if err != nil || p.calls != 1 {
			t.Fatal(err)
		}
	}
}
