package server

import (
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

func TestSessionProtoExposesStructuredInteraction(t *testing.T) {
	now := time.Now().UTC()
	info := &bridge.SessionInfo{Interaction: bridge.Interaction{State: bridge.InteractionWaitingForInput, Revision: 3, UpdatedAt: now, LastActivityAt: now, Pending: &bridge.PendingRequest{ID: "request-1", Type: bridge.PendingRequestInput}, Evidence: bridge.InteractionEvidence{Source: "claude-hooks", Capability: bridge.InteractionCapabilities{InteractionStateSupported: true, ApprovalStateSupported: true}}}}
	got := sessionInfoToProto(info).GetInteraction()
	if got.GetState() != "waiting_for_input" || got.GetRevision() != 3 || got.GetSource() != "claude-hooks" || got.GetPendingRequest().GetId() != "request-1" || !got.GetCapability().GetInteractionStateSupported() || !got.GetUpdatedAt().AsTime().Equal(now) {
		t.Fatalf("lost interaction: %+v", got)
	}
	unknown := sessionInfoToProto(&bridge.SessionInfo{}).GetInteraction()
	if unknown.GetState() != "unknown" || unknown.GetUpdatedAt() != nil || unknown.GetCapability().GetInteractionStateSupported() {
		t.Fatalf("bad default: %+v", unknown)
	}
}
