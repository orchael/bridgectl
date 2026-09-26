package codexapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/orchael/bridgectl/internal/bridge"
)

// MAR-66: decisions are bound to a specific RPC, never PTY input or an item
// that may have more than one approval request during its lifetime.
func TestProxyStructuredApproval(t *testing.T) {
	for _, kind := range []string{"accept", "cancel", "foreign", "resolved", "network", "permissions", "long", "restricted", "local"} {
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
				p["networkApprovalContext"] = map[string]any{"host": "example.com"}
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
			capable := kind == "accept" || kind == "cancel" || kind == "resolved" || kind == "local"
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
