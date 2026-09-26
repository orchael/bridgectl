package codexapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestMAR95ActivityPrivacyAndThreadBinding(t *testing.T) {
	c := &ResponseClient{state: observerState{threadID: "owner"}}
	for _, kind := range []string{"reasoning", "agentMessage", "commandExecution", "fileChange", "mcpToolCall"} {
		for _, thread := range []string{"owner", "other"} {
			c.observeActivityLocked(notif("item/completed", map[string]any{"threadId": thread, "item": map[string]any{"type": kind, "text": "SECRET", "command": "TOKEN=SECRET env", "aggregatedOutput": "SECRET", "summary": []string{"hidden SECRET"}, "changes": []string{"SECRET"}}}))
		}
	}
	w := c.Observe(0, 128, 32768)
	raw, _ := json.Marshal(w)
	if len(w.Events) != 4 || strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "reasoning") {
		t.Fatal(string(raw))
	}
}
func TestMAR95InstructionOwnerRPC(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "steer"}[active], func(t *testing.T) {
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
			serverSend(t, tui, envelope{ID: json.RawMessage(`7`), Method: "thread/start", Params: mustJSON(map[string]any{})})
			serverReadEnvelope(t, upstream)
			forward := func(env envelope) { serverSend(t, upstream, env); serverReadEnvelope(t, tui) }
			forward(envelope{ID: json.RawMessage(`7`), Result: mustJSON(map[string]any{"thread": map[string]any{"id": "owner", "status": map[string]string{"type": "idle"}}})})
			<-client.Updates
			if active {
				forward(notif("turn/started", map[string]any{"threadId": "owner", "turn": map[string]string{"id": "turn"}}))
				forward(notif(methodThreadStatusChanged, map[string]any{"threadId": "owner", "status": map[string]any{"type": "active", "activeFlags": []string{}}}))
				<-client.Updates
			}
			result := make(chan error, 1)
			go func() { result <- client.Instruct(ctx, "Run tests") }()
			request := serverReadEnvelope(t, upstream)
			want := "turn/start"
			if active {
				want = "turn/steer"
			}
			if request.Method != want || !strings.Contains(string(request.Params), `"threadId":"owner"`) || strings.Contains(string(request.Params), "approvalPolicy") {
				t.Fatal(request)
			}
			if active && !strings.Contains(string(request.Params), `"expectedTurnId":"turn"`) {
				t.Fatal(request)
			}
			serverSend(t, upstream, envelope{ID: request.ID, Result: mustJSON(map[string]any{})})
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			// The injected response must be swallowed, the next notification is the
			// next frame the TUI receives. Its own response IDs remain untouched.
			forward(notif("turn/completed", map[string]any{"threadId": "owner", "turn": map[string]string{"id": "turn"}}))
		})
	}
}
