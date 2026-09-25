package codexapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/orchael/bridgectl/internal/bridge"
)

func TestProxyRequestBoundResponse(t *testing.T) {
	for _, kind := range []string{"single", "secret", "multiple", "resolved", "other-thread"} {
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
			forward := func(env envelope) {
				t.Helper()
				serverSend(t, upstream, env)
				got := serverReadEnvelope(t, tui)
				if got.Method != env.Method {
					t.Fatal("relay changed method")
				}
			}
			forward(notif(methodThreadStarted, map[string]any{"thread": map[string]any{"id": "thread", "status": map[string]any{"type": "idle"}}}))
			<-client.Updates
			forward(notif(methodThreadStatusChanged, map[string]any{"threadId": "thread", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnUserInput"}}}))
			<-client.Updates
			questions := []map[string]any{{"id": "question", "question": "Choose a color", "isSecret": kind == "secret"}}
			if kind == "multiple" {
				questions = append(questions, map[string]any{"id": "second"})
			}
			thread := "thread"
			if kind == "other-thread" {
				thread = "foreign"
			}
			forward(envelope{ID: json.RawMessage(`42`), Method: methodItemToolRequestUserInput, Params: mustJSON(map[string]any{"threadId": thread, "itemId": "pending", "questions": questions})})
			if kind != "other-thread" {
				update := <-client.Updates
				if update.State != bridge.InteractionWaitingForInput || update.Pending.ID != "pending" {
					t.Fatalf("bad waiting state: %+v", update)
				}
				if update.Evidence.Capability.RemoteResponseSupported != (kind == "single" || kind == "resolved") {
					t.Fatal("incorrect capability")
				}
			}
			if err := client.Respond(ctx, "stale", "BLUE"); err == nil {
				t.Fatal("stale request accepted")
			}
			if kind == "resolved" {
				forward(notif("serverRequest/resolved", map[string]any{"threadId": "thread", "requestId": 42}))
			}
			err = client.Respond(ctx, "pending", "BLUE")
			if kind != "single" {
				if err == nil {
					t.Fatal("unsupported or resolved input accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response := serverReadEnvelope(t, upstream)
			if string(response.ID) != "42" || string(response.Result) != `{"answers":{"question":{"answers":["BLUE"]}}}` {
				t.Fatalf("wrong RPC response: %+v", response)
			}
			if client.state.pending == nil {
				t.Fatal("transport acceptance cleared pending")
			}
			if err := client.Respond(ctx, "pending", "BLUE"); err == nil {
				t.Fatal("duplicate accepted")
			}
			forward(notif(methodThreadStatusChanged, map[string]any{"threadId": "thread", "status": map[string]any{"type": "active", "activeFlags": []string{}}}))
			update := <-client.Updates
			if update.State != bridge.InteractionWorking || update.Pending != nil {
				t.Fatal("provider acknowledgement did not clear pending")
			}
		})
	}
}
