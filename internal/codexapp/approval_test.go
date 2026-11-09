package codexapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/orchael/bridgectl/internal/bridge"
)

// MAR-66: decisions are bound to a specific RPC, never PTY input or an item
// that may have more than one approval request during its lifetime.
func TestProxyStructuredApproval(t *testing.T) {
	for _, kind := range []string{"accept", "cancel", "foreign", "resolved", "network", "network-extra", "network-badhost", "permissions", "long", "restricted", "local", "racing", "racing-after-resolve", "racing-after-next"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server := newFakeAppServer(t)
			endpoint, client, err := Proxy(ctx, server.wsURL())
			if err != nil {
				t.Fatal(err)
			}
			tui, _, err := websocket.Dial(ctx, endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tui.CloseNow() }()
			upstream := server.acceptConn(t)
			defer func() { _ = upstream.CloseNow() }()
			forward := func(env envelope) { t.Helper(); serverSend(t, upstream, env); serverReadEnvelope(t, tui) }
			serverSend(t, tui, envelope{ID: json.RawMessage(`1`), Method: "thread/start"})
			serverReadEnvelope(t, upstream)
			forward(envelope{ID: json.RawMessage(`1`), Result: mustJSON(map[string]any{"thread": map[string]any{"id": "owner", "status": map[string]any{"type": "idle"}}})})
			<-client.Updates
			forward(notif(methodThreadStatusChanged, map[string]any{"threadId": "owner", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}}}))
			<-client.Updates
			p := map[string]any{"threadId": "owner", "turnId": "turn", "itemId": "item", "command": "printf BLUE", "cwd": "/tmp"}
			switch kind {
			case "foreign":
				p["threadId"] = "other"
			case "network":
				p["networkApprovalContext"] = map[string]any{"host": "github.com", "protocol": "https"}
			case "network-extra":
				p["networkApprovalContext"] = map[string]any{"host": "github.com", "protocol": "https", "scope": "all"}
			case "network-badhost":
				p["networkApprovalContext"] = map[string]any{"host": "evil.com\nRun once", "protocol": "https"}
			case "permissions":
				p["additionalPermissions"] = map[string]any{"network": map[string]any{"enabled": true}}
			case "long":
				p["command"] = string(make([]byte, 600))
			case "restricted":
				p["availableDecisions"] = []string{"cancel"}
			}
			forward(envelope{ID: json.RawMessage(`42`), Method: methodItemCommandExecApproval, Params: mustJSON(p)})
			if kind == "foreign" {
				client.mu.Lock()
				pending := client.state.pending
				client.mu.Unlock()
				if pending != nil {
					t.Fatal("foreign approval polluted session")
				}
				return
			}
			update := <-client.Updates
			capable := kind == "accept" || kind == "cancel" || kind == "resolved" || kind == "local" || kind == "racing" || kind == "racing-after-resolve" || kind == "racing-after-next" || kind == "network"
			if update.Pending == nil || update.Evidence.Capability.StructuredApprovalSupported != capable {
				t.Fatalf("bad capability: %+v", update)
			}
			id := update.Pending.ID
			if err := client.Decide(ctx, "stale", "accept"); err == nil {
				t.Fatal("stale accepted")
			}
			if err := client.Decide(ctx, id, "acceptForSession"); err == nil {
				t.Fatal("persistent grant accepted")
			}
			if kind == "resolved" {
				forward(notif("serverRequest/resolved", map[string]any{"threadId": "owner", "requestId": 42}))
			}
			if kind == "local" {
				serverSend(t, tui, envelope{ID: json.RawMessage(`42`), Result: mustJSON(map[string]string{"decision": "cancel"})})
				serverReadEnvelope(t, upstream)
			}
			decision := "accept"
			if kind == "cancel" {
				decision = "cancel"
			}
			err = client.Decide(ctx, id, decision)
			if !capable || kind == "resolved" || kind == "local" {
				if err == nil {
					t.Fatal("unsupported/resolved accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := serverReadEnvelope(t, upstream)
			if string(got.ID) != "42" || string(got.Result) != `{"decision":"`+decision+`"}` {
				t.Fatalf("wrong decision: %+v", got)
			}
			if err := client.Decide(ctx, id, decision); err == nil {
				t.Fatal("duplicate accepted")
			}
			if kind == "racing-after-resolve" {
				// Codex resolves the request (clearing the current pending one)
				// before the terminal's delayed answer arrives.
				forward(notif("serverRequest/resolved", map[string]any{"threadId": "owner", "requestId": 42}))
			}
			if kind == "racing-after-next" {
				// A new approval replaces the current request before the
				// terminal's delayed answer to the first one arrives.
				next := map[string]any{"threadId": "owner", "turnId": "turn", "itemId": "item2", "command": "printf GREEN", "cwd": "/tmp"}
				forward(envelope{ID: json.RawMessage(`43`), Method: methodItemCommandExecApproval, Params: mustJSON(next)})
			}
			if kind == "racing" || kind == "racing-after-resolve" || kind == "racing-after-next" {
				// The person at the attached terminal answers after the browser
				// already did: the late local answer must not reach Codex.
				serverSend(t, tui, envelope{ID: json.RawMessage(`42`), Result: mustJSON(map[string]string{"decision": "cancel"})})
				quiet, stop := context.WithTimeout(ctx, 300*time.Millisecond)
				_, late, err := upstream.Read(quiet)
				stop()
				if err == nil {
					t.Fatalf("late local answer forwarded after remote decision: %s", late)
				}
				return // the timed-out read closed the fake upstream connection
			}
			client.mu.Lock()
			pending := client.state.pending
			client.mu.Unlock()
			if pending == nil {
				t.Fatal("write cleared pending")
			}
			forward(notif(methodThreadStatusChanged, map[string]any{"threadId": "owner", "status": map[string]any{"type": "active", "activeFlags": []string{}}}))
			update = <-client.Updates
			if update.State != bridge.InteractionWorking || update.Pending != nil {
				t.Fatal("provider did not clear pending")
			}
		})
	}
}

// TestPendingKinds covers the provider-assigned, fixed-vocabulary kind for each
// request shape, which replaces free-text summaries in diagnostics.
func TestPendingKinds(t *testing.T) {
	exec := pendingFromExecApproval(envelope{Params: mustJSON(map[string]any{"callId": "c1", "command": []string{"ls"}})})
	patch := pendingFromApplyPatch(envelope{Params: mustJSON(map[string]any{"callId": "c2"})})
	input := pendingFromUserInput(envelope{Params: mustJSON(map[string]any{"itemId": "i1"})})
	v2 := func(method string) *bridge.PendingRequest {
		return approvalPending(envelope{ID: json.RawMessage(`1`), Method: method}, commandApprovalParams{ThreadID: "t", TurnID: "u", ItemID: "i", Cwd: "/x", Command: "ls"})
	}
	for name, tc := range map[string]struct {
		got  *bridge.PendingRequest
		want bridge.PendingRequestKind
	}{
		"exec": {exec, bridge.PendingKindCommand}, "patch": {patch, bridge.PendingKindFileChange},
		"input":          {input, bridge.PendingKindQuestion},
		"v2 command":     {v2(methodItemCommandExecApproval), bridge.PendingKindCommand},
		"v2 file change": {v2(methodItemFileChangeApproval), bridge.PendingKindFileChange},
		"v2 permissions": {v2(methodItemPermissionsApproval), bridge.PendingKindOther},
	} {
		if tc.got == nil || tc.got.Kind != tc.want {
			t.Errorf("%s: %+v, want kind %q", name, tc.got, tc.want)
		}
	}
}

// Usability review: a command approval that Bridge cannot answer remotely
// (network/permission escalation, long command, restricted decisions) must
// still say what is being approved, so the person knows what to attach for.
// It must never use the actionable "Run once in" wording, and it must stay
// within summaryCap without control characters.
func TestApprovalSummaryAlwaysNamesTheCommand(t *testing.T) {
	env := envelope{ID: json.RawMessage(`7`), Method: methodItemCommandExecApproval}
	base := commandApprovalParams{ThreadID: "t", TurnID: "u", ItemID: "i", Cwd: "/Users/mark/src/bilge-pump", Command: "git push origin feat/cache-cleaner-meta-tool"}
	long := strings.Repeat("x", 600)
	for name, tc := range map[string]struct {
		mutate     func(*commandApprovalParams)
		wantPrefix string
		wantText   string
	}{
		"actionable": {func(p *commandApprovalParams) {}, "Run once in /Users/mark/src/bilge-pump: ", "git push origin feat/cache-cleaner-meta-tool"},
		"network": {func(p *commandApprovalParams) {
			p.Network = json.RawMessage(`{"host":"github.com","protocol":"https"}`)
		}, "Run once with https access to github.com in /Users/mark/src/bilge-pump: ", "git push origin feat/cache-cleaner-meta-tool"},
		"network-bad": {func(p *commandApprovalParams) { p.Network = json.RawMessage(`{"host":"github.com"}`) }, "Needs network access: git push origin feat/cache-cleaner-meta-tool", " · in /Users/mark/src/bilge-pump"},
		"permissions": {func(p *commandApprovalParams) { p.Permissions = json.RawMessage(`{"network":{"enabled":true}}`) }, "Needs extra permissions: git push origin", " · in /Users/mark/src/bilge-pump"},
		"restricted":  {func(p *commandApprovalParams) { p.AvailableDecisions = []json.RawMessage{json.RawMessage(`"cancel"`)} }, "Approve in Codex: git push origin", " · in /Users/mark/src/bilge-pump"},
		"long":        {func(p *commandApprovalParams) { p.Command = "echo " + long }, "Approve in Codex: echo xxx", "…"},
		"multiline":   {func(p *commandApprovalParams) { p.Command = "git add .\ngit push\x1b[31m" }, "Approve in Codex: git add . git push", ""},
		// The command must survive a working directory longer than the whole cap;
		// the directory is shortened from the left instead.
		"long cwd": {func(p *commandApprovalParams) {
			p.Network = json.RawMessage(`{"host":"github.com"}`)
			p.Cwd = "/" + strings.Repeat("deep/", 60) + "bilge-pump"
		}, "Needs network access: git push origin feat/cache-cleaner-meta-tool · in …", "bilge-pump"},
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			got := approvalPending(env, p)
			if got == nil {
				t.Fatal("no pending request")
			}
			if !strings.HasPrefix(got.Summary, tc.wantPrefix) || !strings.Contains(got.Summary, tc.wantText) {
				t.Fatalf("summary %q, want prefix %q containing %q", got.Summary, tc.wantPrefix, tc.wantText)
			}
			if name != "actionable" && name != "network" && strings.HasPrefix(got.Summary, "Run once") {
				t.Fatalf("non-actionable request uses actionable wording: %q", got.Summary)
			}
			if len(got.Summary) > summaryCap || !utf8.ValidString(got.Summary) || bridge.ValidateResponse(got.Summary) != nil || strings.ContainsAny(got.Summary, "\n\x1b") {
				t.Fatalf("summary not safe for display: %q (%d bytes)", got.Summary, len(got.Summary))
			}
		})
	}
	// Approvals without a command (file changes, permission grants) keep the generic label.
	other := approvalPending(envelope{ID: json.RawMessage(`8`), Method: methodItemFileChangeApproval}, commandApprovalParams{ThreadID: "t", TurnID: "u", ItemID: "i"})
	if other == nil || other.Summary != "Approval required in Codex" {
		t.Fatalf("file change summary: %+v", other)
	}
}
