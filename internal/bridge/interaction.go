package bridge

import (
	"context"
	"time"
)

// InteractionStateValue is the provider-neutral, authoritative-only
// interaction state of a session: what the agent is doing right now from the
// user's point of view. It is a distinct axis from SessionState (process
// lifecycle) and from control-plane connectivity, and must never be inferred
// from either: a running process is not necessarily "working", and an
// online control connection says nothing about whether the agent is
// actively doing anything.
//
// This set is intentionally small and extensible; adding a new value here
// does not require changing SessionState or any transport that already
// forwards InteractionStateValue as an opaque string.
type InteractionStateValue string

const (
	// InteractionUnknown is the only state a provider without interaction
	// capability may report. It is also the zero value: a session that has
	// never received an authoritative interaction update reports Unknown,
	// never a guess.
	InteractionUnknown InteractionStateValue = "unknown"
	// InteractionWorking means the agent is actively processing (generating,
	// running a tool, editing) and is not blocked on the user.
	InteractionWorking InteractionStateValue = "working"
	// InteractionWaitingForInput means the provider has authoritatively
	// reported that it is blocked on a free-form answer from the user.
	InteractionWaitingForInput InteractionStateValue = "waiting_for_input"
	// InteractionWaitingForApproval means the provider has authoritatively
	// reported that it is blocked on the user approving or denying a
	// specific action (e.g. running a command, applying a patch).
	InteractionWaitingForApproval InteractionStateValue = "waiting_for_approval"
	// InteractionIdle means the agent finished its current turn and is not
	// blocked on any specific pending request; it is simply not doing
	// anything right now (e.g. between turns in a chat-style session).
	InteractionIdle InteractionStateValue = "idle"
)

// PendingRequestType identifies why a session is blocked on the user.
type PendingRequestType string

const (
	PendingRequestInput    PendingRequestType = "input"
	PendingRequestApproval PendingRequestType = "approval"
)

// PendingRequest is a stable-identity request for user attention.
//
// ID must be stable for the lifetime of the request: the same logical
// question or approval must keep the same ID across repeated authoritative
// reports, so a consumer (bridgectl's control client, Bridge) can tell "this
// is still the same pending request" apart from "the provider resolved one
// request and immediately raised a new one". It is cleared only by
// UpdateInteraction being called again with authoritative evidence that the
// request was resolved or superseded — never as a side effect of writing
// bytes to a PTY. See Supervisor.WriteInput and Supervisor.UpdateInteraction.
type PendingRequest struct {
	ID   string
	Type PendingRequestType
	// Summary is an optional, short, privacy-safe description of the pending
	// request, supplied verbatim by the provider (e.g. a tool-call name or a
	// structured prompt title). It must never be derived from raw terminal
	// output, chain-of-thought, or free-form transcript text: if the
	// provider does not supply a safe structured summary, this stays empty
	// and consumers fall back to a generic label such as "Waiting for your
	// answer".
	Summary string
}

// InteractionCapabilities declares what a provider can authoritatively
// report about interaction state. Every field defaults to false: a provider
// that does not implement InteractionCapableProvider is treated exactly as
// if every field here were false, which is what makes InteractionUnknown
// its only honest report.
type InteractionCapabilities struct {
	// InteractionStateSupported means the provider can authoritatively
	// distinguish at least Working from Idle/Unknown for its sessions.
	InteractionStateSupported bool
	// ApprovalStateSupported means the provider can authoritatively raise
	// InteractionWaitingForApproval with a PendingRequest (not merely
	// WaitingForInput used loosely for both cases).
	ApprovalStateSupported bool
	// PendingSummarySupported means the provider can supply a privacy-safe
	// PendingRequest.Summary rather than leaving it empty.
	PendingSummarySupported bool
}

// InteractionEvidence records where an interaction-state value came from, so
// nothing downstream has to guess whether a given report is authoritative.
type InteractionEvidence struct {
	// Source identifies the concrete signal that produced this report, e.g.
	// "claude-stream-json", "opencode-sse". Empty Source is only valid for
	// the zero-value/Unknown Interaction that a session starts with.
	Source string
	// Capability is the reporting provider's declared capability at the time
	// of this report.
	Capability InteractionCapabilities
}

// Interaction is the current authoritative interaction state of a session.
// The zero value (State == "") is treated identically to InteractionUnknown
// by State's accessor below.
type Interaction struct {
	State InteractionStateValue
	// Revision is a local, monotonically increasing sequence number bumped
	// only when State or Pending's identity actually changes (never on a
	// repeated report of the same value, and never reset except by a new
	// session). Transports that forward Interaction downstream (e.g.
	// bridgecontrol) allocate their own wire-level, restart-durable revision
	// from this signal; see internal/bridgecontrol/observer.go.
	Revision uint64
	// UpdatedAt is when State (or Pending's identity) last actually changed.
	UpdatedAt time.Time
	// LastActivityAt is when bridgectl last received any authoritative
	// interaction report for this session, even one that repeats the
	// current State. It is refreshed on every UpdateInteraction call and is
	// what lets a consumer tell "still confirmed current" apart from "we
	// simply haven't heard anything new" (the latter is a connectivity/
	// staleness concern, not an interaction-state one).
	LastActivityAt time.Time
	// Pending is non-nil only while State is WaitingForInput or
	// WaitingForApproval and the provider has supplied a stable request
	// identity for it.
	Pending  *PendingRequest
	Evidence InteractionEvidence
}

// EffectiveState returns State, normalizing the zero value to
// InteractionUnknown so callers never have to special-case an empty string.
func (i Interaction) EffectiveState() InteractionStateValue {
	if i.State == "" {
		return InteractionUnknown
	}
	return i.State
}

// samePendingIdentity reports whether next represents the same logical
// pending request as the receiver: both nil, or both non-nil with equal ID
// and Type. A changed Summary alone (same ID/Type) is not an identity
// change; see equalForRevision, which does treat a Summary change as
// meaningful enough to bump the revision.
func (p *PendingRequest) samePendingIdentity(next *PendingRequest) bool {
	if p == nil || next == nil {
		return p == nil && next == nil
	}
	return p.ID == next.ID && p.Type == next.Type
}

// changed reports whether next differs from the receiver in any way a
// consumer should be told about: a different State, a different Pending
// request identity, or the same Pending request with an updated Summary.
func (i Interaction) changed(next Interaction) bool {
	if i.EffectiveState() != next.EffectiveState() {
		return true
	}
	if !i.Pending.samePendingIdentity(next.Pending) {
		return true
	}
	if i.Pending != nil && next.Pending != nil && i.Pending.Summary != next.Pending.Summary {
		return true
	}
	return false
}

// InteractionCapableProvider is implemented by providers that can report
// authoritative interaction state for the sessions they run. A provider that
// does not implement this interface is treated as fully unsupported:
// Supervisor never guesses on its behalf, and every session it runs reports
// InteractionUnknown with a zero-value InteractionCapabilities.
type InteractionCapableProvider interface {
	InteractionCapabilities() InteractionCapabilities
}

// interactionCapabilitiesFor returns provider's declared capabilities, or
// the all-false zero value if it does not implement
// InteractionCapableProvider.
func interactionCapabilitiesFor(provider Provider) InteractionCapabilities {
	if icp, ok := provider.(InteractionCapableProvider); ok {
		return icp.InteractionCapabilities()
	}
	return InteractionCapabilities{}
}

// InteractionWatcher is implemented by a provider that can supply a live
// stream of authoritative interaction-state updates for a session out of
// band from however its PTY/stdout is otherwise structured — for example a
// companion protocol connection alongside an ordinary interactive PTY
// process (see internal/provider's Codex app-server observer). A provider
// that also implements InteractionCapableProvider should keep the two
// consistent: the capability it declares should describe exactly what this
// watch stream can report.
//
// Supervisor starts watching immediately after the session's process is
// successfully started and stops when the session's context is cancelled
// (on Stop, on process exit, or on daemon shutdown). WatchInteraction must
// return promptly; any setup it needs (dialing a companion process, etc.)
// happens after return, inside the goroutine that drains the returned
// channel, so a slow or failing watch connection never delays session
// startup or blocks WriteInput/Attach.
type InteractionWatcher interface {
	WatchInteraction(ctx context.Context, sessionID string, cfg SessionConfig) (<-chan Interaction, error)
}
