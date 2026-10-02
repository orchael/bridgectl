package codexapp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/orchael/bridgectl/internal/bridge"
)

// observeActivityLocked deliberately reads no free-form provider content.
// An allowlist of lifecycle labels excludes reasoning, command arguments,
// tool output, environment, credentials and filesystem contents by construction.
func (c *ResponseClient) observeActivityLocked(env envelope) {
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
		Item struct {
			Type     string `json:"type"`
			Status   string `json:"status"`
			ExitCode *int   `json:"exitCode"`
		} `json:"item"`
	}
	if json.Unmarshal(env.Params, &p) != nil || c.state.threadID == "" || p.ThreadID != c.state.threadID {
		return
	}
	label, kind := "", "status"
	switch env.Method {
	case "turn/started":
		c.turnID = p.Turn.ID
		label = "Agent started work"
	case "turn/completed":
		c.turnID = ""
		label = "Agent finished the turn"
	case "item/started", "item/completed":
		labels := map[string]string{"agentMessage": "Assistant message", "commandExecution": "Command execution", "fileChange": "File change", "mcpToolCall": "Tool call", "dynamicToolCall": "Tool call", "webSearch": "Web search"}
		label = labels[p.Item.Type]
		if label == "" {
			return
		}
		kind = "tool"
		if p.Item.Type == "agentMessage" {
			kind = "message"
		}
		if env.Method == "item/started" {
			label += " started"
		} else {
			label += " completed"
			if p.Item.Status == "failed" || (p.Item.ExitCode != nil && *p.Item.ExitCode != 0) {
				label += " with an error"
			}
		}
	}
	if label != "" {
		c.activity.Append(kind, label, time.Now())
	}
}
func (c *ResponseClient) Observe(after uint64, events, bytes int) bridge.ActivityWindow {
	w := c.activity.Window(after, events, bytes, time.Now())
	c.mu.Lock()
	w.InstructionSupported = !c.closed && c.conn != nil && c.state.threadID != ""
	c.mu.Unlock()
	return w
}

// Instruct sends to the existing owner thread. It waits for the actual RPC
// result; an uncertain write/result remains unknown in the command ledger.
func (c *ResponseClient) Instruct(ctx context.Context, text string) error {
	if err := bridge.ValidateResponse(text); err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed || c.conn == nil || c.state.threadID == "" {
		c.mu.Unlock()
		return bridge.ErrRemoteResponseUnsupported
	}
	method := "turn/start"
	params := map[string]any{"threadId": c.state.threadID, "input": []any{map[string]any{"type": "text", "text": text, "text_elements": []any{}}}}
	if c.state.status.Type == threadStatusActive && len(c.state.status.ActiveFlags) == 0 && c.turnID != "" {
		method = "turn/steer"
		params["expectedTurnId"] = c.turnID
	} else if c.state.status.Type != threadStatusIdle {
		c.mu.Unlock()
		return bridge.ErrPendingRequestMismatch
	}
	id := mustJSON("bridgectl-" + uuid.NewString())
	ch := make(chan error, 1)
	if c.instructionResults == nil {
		c.instructionResults = make(map[string]chan error)
	}
	c.instructionResults[string(id)] = ch
	conn := c.conn
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.instructionResults, string(id)); c.mu.Unlock() }()
	if err := conn.Write(ctx, websocket.MessageText, mustJSON(envelope{JSONRPC: "2.0", ID: id, Method: method, Params: mustJSON(params)})); err != nil {
		return err
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
