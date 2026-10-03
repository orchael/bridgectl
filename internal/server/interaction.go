package server

import (
	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/bridge"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func interactionToProto(i bridge.Interaction) *bridgev1.SessionInteraction {
	c := i.Evidence.Capability
	out := &bridgev1.SessionInteraction{
		State: string(i.EffectiveState()), Revision: i.Revision, Source: i.Evidence.Source,
		Capability: &bridgev1.InteractionCapability{
			InteractionStateSupported: c.InteractionStateSupported, ApprovalStateSupported: c.ApprovalStateSupported,
			PendingSummarySupported: c.PendingSummarySupported, RemoteResponseSupported: c.RemoteResponseSupported, StructuredApprovalSupported: c.StructuredApprovalSupported,
		},
	}
	if !i.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(i.UpdatedAt)
	}
	if !i.LastActivityAt.IsZero() {
		out.LastActivityAt = timestamppb.New(i.LastActivityAt)
	}
	if i.Pending != nil {
		out.PendingRequest = &bridgev1.PendingInteractionRequest{Id: i.Pending.ID, Type: string(i.Pending.Type), Summary: i.Pending.Summary, Kind: string(i.Pending.Kind)}
	}
	return out
}
