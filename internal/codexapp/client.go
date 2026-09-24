package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/orchael/bridgectl/internal/bridge"
)

// Capabilities is what this client can authoritatively report for a Codex
// session observed through app-server: full interaction state, approval
// state, and a privacy-safe pending-request summary derived from the
// approval/input request's own structured fields (never raw PTY output).
var Capabilities = bridge.InteractionCapabilities{
	InteractionStateSupported: true,
	ApprovalStateSupported:    true,
	PendingSummarySupported:   true,
}

const evidenceSource = "codex-app-server"

// dialTimeout/readTimeout bound the observer connection; a companion
// app-server that never becomes reachable, or one that goes silent, must
// never hang the session it's attached to.
const (
	dialTimeout    = 10 * time.Second
	idleReadWindow = 60 * time.Second
)

// Watch connects to a `codex app-server --listen ws://...` instance,
// completes the initialize handshake, and emits a bridge.Interaction value
// on the returned channel every time the observed thread's authoritative
// status changes (see threadStatusToInteraction). It never sends turn/start
// or an approval/input response — see the package doc comment.
//
// The channel is closed when ctx is cancelled or the connection is
// unrecoverably lost. Watch itself does not retry the WebSocket dial in a
// loop forever; a single connection attempt is made (the companion process
// this connects to is started by the same session it observes, so a dial
// failure or a drop past the point of no return means the session's
// process group is already going away).
func Watch(ctx context.Context, wsURL string, logger *slog.Logger) (<-chan bridge.Interaction, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("codexapp: dial %s: %w", wsURL, err)
	}

	if err := sendInitialize(ctx, conn); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "initialize failed")
		return nil, err
	}

	out := make(chan bridge.Interaction, 8)
	go runObserver(ctx, conn, out, logger)
	return out, nil
}

func sendInitialize(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	req := envelope{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  methodInitialize,
		Params: mustJSON(initializeParams{ClientInfo: clientInfo{
			Name: "bridgectl-observer", Version: "1",
		}}),
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("codexapp: send initialize: %w", err)
	}
	// Read until we see the response to id=1 (a notification, e.g.
	// remoteControl/status/changed, may legitimately arrive first — see the
	// real app-server capture in docs/codex-app-server-observer.md).
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("codexapp: read initialize response: %w", err)
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if len(env.ID) > 0 && env.Method == "" {
			if env.Error != nil {
				return fmt.Errorf("codexapp: initialize rejected: %s", env.Error.Message)
			}
			return nil
		}
	}
}

// observerState tracks just enough to translate the wire protocol into
// Interaction values: which thread we've adopted (the first one seen — a
// single interactive TUI session creates exactly one), and the most
// recently seen pending approval/input request (for identity/summary only;
// authoritative clearing still comes solely from a later ThreadStatus that
// no longer carries the corresponding activeFlag).
type observerState struct {
	threadID string
	pending  *bridge.PendingRequest
}

func runObserver(ctx context.Context, conn *websocket.Conn, out chan<- bridge.Interaction, logger *slog.Logger) {
	defer close(out)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	st := &observerState{}
	for {
		readCtx, cancel := context.WithTimeout(ctx, idleReadWindow)
		_, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				logger.Debug("codexapp: observer connection ended", "error", err)
			}
			return
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Method == "" {
			continue // a response to a request we never sent (shouldn't happen)
		}
		interaction, ok := handleMessage(st, env, logger)
		if !ok {
			continue
		}
		select {
		case out <- interaction:
		case <-ctx.Done():
			return
		}
	}
}

// handleMessage updates st from one incoming notification/request and
// returns the Interaction to emit, if this message is authoritative
// evidence of a state that should be reported.
func handleMessage(st *observerState, env envelope, logger *slog.Logger) (bridge.Interaction, bool) {
	switch env.Method {
	case methodThreadStarted:
		var p threadStartedParams
		if err := json.Unmarshal(env.Params, &p); err != nil || p.Thread.ID == "" {
			return bridge.Interaction{}, false
		}
		if st.threadID == "" {
			st.threadID = p.Thread.ID
		}
		if p.Thread.ID != st.threadID {
			return bridge.Interaction{}, false
		}
		return statusToInteraction(st, p.Thread.Status), true

	case methodThreadStatusChanged:
		var p threadStatusChangedParams
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return bridge.Interaction{}, false
		}
		if st.threadID == "" {
			st.threadID = p.ThreadID // observer connected after thread/started already fired
		}
		if p.ThreadID != st.threadID {
			return bridge.Interaction{}, false
		}
		return statusToInteraction(st, p.Status), true

	case methodExecCommandApproval, methodItemCommandExecApproval:
		st.pending = pendingFromExecApproval(env)
		return bridge.Interaction{}, false // wait for the ThreadStatus that follows

	case methodApplyPatchApproval, methodItemFileChangeApproval:
		st.pending = pendingFromApplyPatch(env)
		return bridge.Interaction{}, false

	case methodItemPermissionsApproval:
		st.pending = pendingFromGeneric(env, bridge.PendingRequestApproval, "Approval required")
		return bridge.Interaction{}, false

	case methodItemToolRequestUserInput:
		st.pending = pendingFromUserInput(env)
		return bridge.Interaction{}, false

	default:
		return bridge.Interaction{}, false
	}
}

// statusToInteraction is the sole place authoritative Codex ThreadStatus
// values become bridge.InteractionStateValue — grounded directly in the
// real ThreadStatus/ThreadActiveFlag enum (see protocol.go's doc comment):
// idle -> Idle, active with waitingOnApproval/waitingOnUserInput -> the
// matching waiting state (with the last-seen pending request attached),
// plain active -> Working, anything else -> Unknown. A transition away from
// a waiting flag (to plain active, or to idle) is itself the authoritative
// evidence that clears Pending — never inferred from anything bridgectl did
// locally.
func statusToInteraction(st *observerState, status threadStatusPayload) bridge.Interaction {
	now := time.Now().UTC()
	interaction := bridge.Interaction{
		UpdatedAt:      now,
		LastActivityAt: now,
		Evidence:       bridge.InteractionEvidence{Source: evidenceSource, Capability: Capabilities},
	}
	switch status.Type {
	case threadStatusIdle:
		interaction.State = bridge.InteractionIdle
		st.pending = nil
	case threadStatusActive:
		switch {
		case hasFlag(status.ActiveFlags, activeFlagWaitingOnApproval):
			interaction.State = bridge.InteractionWaitingForApproval
			interaction.Pending = st.pending
		case hasFlag(status.ActiveFlags, activeFlagWaitingOnUserInput):
			interaction.State = bridge.InteractionWaitingForInput
			interaction.Pending = st.pending
		default:
			interaction.State = bridge.InteractionWorking
			st.pending = nil
		}
	case threadStatusNotLoaded, threadStatusSystemError:
		interaction.State = bridge.InteractionUnknown
		st.pending = nil
	default:
		interaction.State = bridge.InteractionUnknown
		st.pending = nil
	}
	return interaction
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// summaryCap bounds any derived summary to a short label, matching
// bridge.PendingRequest.Summary's privacy contract (a label, not a
// transcript) and Bridge's own pending_request_summary column limit.
const summaryCap = 200

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func pendingFromExecApproval(env envelope) *bridge.PendingRequest {
	var p execCommandApprovalParams
	if err := json.Unmarshal(env.Params, &p); err != nil || p.CallID == "" {
		return nil
	}
	summary := "Run a command"
	if len(p.Command) > 0 {
		summary = "Run: " + truncate(strings.Join(p.Command, " "), summaryCap)
	}
	return &bridge.PendingRequest{ID: p.CallID, Type: bridge.PendingRequestApproval, Summary: summary}
}

func pendingFromApplyPatch(env envelope) *bridge.PendingRequest {
	var p applyPatchApprovalParams
	if err := json.Unmarshal(env.Params, &p); err != nil || p.CallID == "" {
		return nil
	}
	paths := make([]string, 0, len(p.FileChanges))
	for _, fc := range p.FileChanges {
		if fc.Path != "" {
			paths = append(paths, fc.Path)
		}
	}
	summary := "Apply a patch"
	if len(paths) > 0 {
		summary = "Apply changes to: " + truncate(strings.Join(paths, ", "), summaryCap)
	}
	return &bridge.PendingRequest{ID: p.CallID, Type: bridge.PendingRequestApproval, Summary: summary}
}

func pendingFromGeneric(env envelope, typ bridge.PendingRequestType, fallback string) *bridge.PendingRequest {
	var p itemApprovalParams
	id := ""
	if err := json.Unmarshal(env.Params, &p); err == nil {
		if p.ItemID != "" {
			id = p.ItemID
		} else if p.CallID != "" {
			id = p.CallID
		}
	}
	if id == "" {
		return nil
	}
	return &bridge.PendingRequest{ID: id, Type: typ, Summary: fallback}
}

func pendingFromUserInput(env envelope) *bridge.PendingRequest {
	var p toolRequestUserInputParams
	if err := json.Unmarshal(env.Params, &p); err != nil || p.ItemID == "" {
		return nil
	}
	summary := "Waiting for your answer"
	if len(p.Questions) > 0 && p.Questions[0].Text != "" {
		summary = truncate(p.Questions[0].Text, summaryCap)
	}
	return &bridge.PendingRequest{ID: p.ItemID, Type: bridge.PendingRequestInput, Summary: summary}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
