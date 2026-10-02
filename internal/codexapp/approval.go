package codexapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/orchael/bridgectl/internal/bridge"
)

type commandApprovalParams struct {
	ThreadID           string            `json:"threadId"`
	TurnID             string            `json:"turnId"`
	ItemID             string            `json:"itemId"`
	Command            string            `json:"command"`
	Cwd                string            `json:"cwd"`
	AvailableDecisions []json.RawMessage `json:"availableDecisions"`
	Network            json.RawMessage   `json:"networkApprovalContext"`
	Permissions        json.RawMessage   `json:"additionalPermissions"`
}

func approvalSummary(p commandApprovalParams) string {
	return "Run once in " + p.Cwd + ": " + p.Command
}
func approvalPending(env envelope, p commandApprovalParams) *bridge.PendingRequest {
	if len(env.ID) == 0 || p.ItemID == "" || p.TurnID == "" {
		return nil
	}
	// An item can ask more than once; use the RPC identity as well as the thread,
	// turn and item. Raw provider IDs never become filesystem paths.
	sum := sha256.Sum256(mustJSON([]string{p.ThreadID, p.TurnID, p.ItemID, string(env.ID)}))
	summary := "Approval required in Codex"
	if approvalSupported(env, p) {
		summary = approvalSummary(p)
	}
	return &bridge.PendingRequest{ID: "approval:" + hex.EncodeToString(sum[:]), Type: bridge.PendingRequestApproval, Summary: summary}
}
func approvalSupported(env envelope, p commandApprovalParams) bool {
	if env.Method != methodItemCommandExecApproval || len(env.ID) == 0 || p.ThreadID == "" || p.TurnID == "" || p.ItemID == "" || strings.TrimSpace(p.Cwd) == "" || bridge.ValidateResponse(p.Command) != nil {
		return false
	}
	summary := approvalSummary(p)
	// Never offer approval of truncated commands or hidden permission scopes.
	if len(summary) > summaryCap || bridge.ValidateResponse(summary) != nil {
		return false
	}
	for _, field := range []json.RawMessage{p.Network, p.Permissions} {
		if len(field) > 0 && string(field) != "null" {
			return false
		}
	}
	if p.AvailableDecisions != nil {
		accept, cancelled := false, false
		for _, raw := range p.AvailableDecisions {
			accept = accept || string(raw) == `"accept"`
			cancelled = cancelled || string(raw) == `"cancel"`
		}
		if !accept || !cancelled {
			return false
		}
	}
	return true
}
