package codexapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

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
	if network, ok := parseNetworkContext(p.Network); ok && network != nil {
		return "Run once with " + network.Protocol + " access to " + network.Host + " in " + p.Cwd + ": " + p.Command
	}
	return "Run once in " + p.Cwd + ": " + p.Command
}

type networkContext struct {
	Host     string `json:"host"`
	Protocol string `json:"protocol"`
}

// parseNetworkContext accepts only Codex's exact managed-network approval
// shape, {host, protocol}, with a plain host name or IP literal and a known
// protocol. Anything else (extra fields, odd characters, unknown protocols)
// stays non-actionable, so the browser can never approve a scope it doesn't
// display. A missing or null context returns (nil, true).
func parseNetworkContext(raw json.RawMessage) (*networkContext, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 || fields["host"] == nil || fields["protocol"] == nil {
		return nil, false
	}
	var n networkContext
	if json.Unmarshal(raw, &n) != nil || !validNetworkHost(n.Host) {
		return nil, false
	}
	switch n.Protocol {
	case "http", "https", "socks5Tcp", "socks5Udp":
		return &n, true
	}
	return nil, false
}

func validNetworkHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, r := range host {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-:[]", r)
		if !ok {
			return false
		}
	}
	return true
}

// informationalSummary names a command approval Bridge cannot answer
// remotely, so the person knows what they are attaching to approve. It never
// uses the actionable "Run once in" wording, flattens control characters, and
// fits summaryCap (Bridge's column limit). The command comes first so it
// survives truncation; the working directory gets whatever space is left and
// is shortened from the left, keeping the most specific path segments.
func informationalSummary(p commandApprovalParams) string {
	reason := "Approve in Codex"
	switch {
	case len(p.Network) > 0 && string(p.Network) != "null":
		reason = "Needs network access"
	case len(p.Permissions) > 0 && string(p.Permissions) != "null":
		reason = "Needs extra permissions"
	}
	head := capRunes(reason+": "+displayText(p.Command), summaryCap)
	cwd := displayText(p.Cwd)
	const sep = " · in "
	if cwd == "" || strings.HasSuffix(head, "…") {
		return head
	}
	room := summaryCap - len(head) - len(sep)
	if room >= len(cwd) {
		return head + sep + cwd
	}
	const ellipsis = "…"
	if room <= len(ellipsis)+1 {
		return head
	}
	start := len(cwd) - (room - len(ellipsis))
	for start < len(cwd) && !utf8.RuneStart(cwd[start]) {
		start++
	}
	return head + sep + ellipsis + cwd[start:]
}

// displayText collapses whitespace (including newlines) to single spaces and
// drops every other control character, so terminal escapes never reach a summary.
func displayText(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r) || r == utf8.RuneError:
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// capRunes truncates to at most limit bytes on a rune boundary, ending with
// an ellipsis when anything was cut.
func capRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…"
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
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
	} else if env.Method == methodItemCommandExecApproval && strings.TrimSpace(p.Command) != "" {
		summary = informationalSummary(p)
	}
	return &bridge.PendingRequest{ID: "approval:" + hex.EncodeToString(sum[:]), Type: bridge.PendingRequestApproval, Summary: summary, Kind: approvalKind(env.Method)}
}

// approvalKind classifies a v2 approval request from its JSON-RPC method (the
// structured signal), since command, file-change and permission approvals all
// flow through approvalPending.
func approvalKind(method string) bridge.PendingRequestKind {
	switch method {
	case methodItemCommandExecApproval:
		return bridge.PendingKindCommand
	case methodItemFileChangeApproval:
		return bridge.PendingKindFileChange
	default: // permission grants and any future approval method
		return bridge.PendingKindOther
	}
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
	// A managed-network request is actionable only in its exact, displayed
	// shape (the host and protocol are in the summary). Additional permission
	// profiles are never actionable remotely.
	if _, ok := parseNetworkContext(p.Network); !ok {
		return false
	}
	if len(p.Permissions) > 0 && string(p.Permissions) != "null" {
		return false
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
