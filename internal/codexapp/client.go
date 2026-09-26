package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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

const dialTimeout = 10 * time.Second

// ResponseClient shares request identity with the observer's single reader.
// Responses are JSON-RPC answers to an outstanding request, never turn/start
// or terminal bytes. The server rejects resolutions of requests already gone.
type ResponseClient struct {
	mu         sync.Mutex
	conn       *websocket.Conn
	state      observerState
	requestID  json.RawMessage
	questionID string
	approval   bool
	submitted  bool
	closed     bool
	Updates    <-chan bridge.Interaction
}

func (c *ResponseClient) Respond(ctx context.Context, pendingID, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.state.pending == nil || c.state.pending.ID != pendingID || c.questionID == "" || len(c.requestID) == 0 || c.submitted {
		return bridge.ErrPendingRequestMismatch
	}
	// Burn the request before I/O: an ambiguous network failure is not safe
	// to retry. Provider evidence, never this write, clears attention.
	c.submitted = true
	b := mustJSON(envelope{JSONRPC: "2.0", ID: c.requestID, Result: mustJSON(map[string]any{"answers": map[string]any{c.questionID: map[string]any{"answers": []string{text}}}})})
	return c.conn.Write(ctx, websocket.MessageText, b)
}

// Decide answers only an explicitly supported command approval, once.
func (c *ResponseClient) Decide(ctx context.Context, pendingID, decision string) error {
	if decision != "accept" && decision != "cancel" {
		return bridge.ErrInvalidResponse
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.approval || c.state.pending == nil || c.state.pending.ID != pendingID || c.submitted || len(c.requestID) == 0 {
		return bridge.ErrPendingRequestMismatch
	}
	c.submitted = true
	return c.conn.Write(ctx, websocket.MessageText, mustJSON(envelope{JSONRPC: "2.0", ID: c.requestID, Result: mustJSON(map[string]string{"decision": decision})}))
}

// Watch provides a read-only secondary status subscription. The provider uses
// Proxy instead so pending requests are observed on the owning connection.
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
		}, Capabilities: map[string]bool{"experimentalApi": true}}),
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
	status   threadStatusPayload
}

func runObserver(ctx context.Context, conn *websocket.Conn, out chan<- bridge.Interaction, logger *slog.Logger) {
	defer close(out)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	st := &observerState{}
	for {
		_, data, err := conn.Read(ctx)
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

	case methodItemCommandExecApproval, methodItemFileChangeApproval, methodItemPermissionsApproval:
		var p commandApprovalParams
		if json.Unmarshal(env.Params, &p) != nil || p.ThreadID != st.threadID {
			return bridge.Interaction{}, false
		}
		st.pending = approvalPending(env, p)
		if hasFlag(st.status.ActiveFlags, activeFlagWaitingOnApproval) {
			return statusToInteraction(st, st.status), true
		}
		return bridge.Interaction{}, false
	case methodExecCommandApproval:
		st.pending = pendingFromExecApproval(env)
		return bridge.Interaction{}, false // wait for the ThreadStatus that follows

	case methodApplyPatchApproval:
		st.pending = pendingFromApplyPatch(env)
		return bridge.Interaction{}, false

	case methodItemToolRequestUserInput:
		var p toolRequestUserInputParams
		if json.Unmarshal(env.Params, &p) != nil || p.ThreadID != st.threadID {
			return bridge.Interaction{}, false
		}
		st.pending = pendingFromUserInput(env)
		if hasFlag(st.status.ActiveFlags, activeFlagWaitingOnUserInput) {
			return statusToInteraction(st, st.status), true
		}
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
	st.status = status
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
