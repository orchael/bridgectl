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
			// History can arrive before the owning start result; it cannot
			// choose the observed session even when it contains a thread.
			forward(envelope{ID: json.RawMessage(`99`), Result: mustJSON(map[string]any{"thread": map[string]any{"id": "history", "status": map[string]any{"type": "notLoaded"}}})})
			serverSend(t, tui, envelope{ID: json.RawMessage(`7`), Method: "thread/start", Params: mustJSON(map[string]any{})})
			_ = serverReadEnvelope(t, upstream)
			forward(envelope{ID: json.RawMessage(`7`), Result: mustJSON(map[string]any{"thread": map[string]any{"id": "thread", "status": map[string]any{"type": "idle"}}})})
			initial := <-client.Updates
			if initial.State != bridge.InteractionIdle {
				t.Fatalf("history adopted as session: %+v", initial)
			}
			// Codex starts ephemeral background threads (e.g. title generation)
			// on this same connection. They must not replace the user's thread.
			serverSend(t, tui, envelope{ID: json.RawMessage(`8`), Method: "thread/start", Params: mustJSON(map[string]any{"ephemeral": true})})
			_ = serverReadEnvelope(t, upstream)
			forward(envelope{ID: json.RawMessage(`8`), Result: mustJSON(map[string]any{"thread": map[string]any{"id": "background", "ephemeral": true, "status": map[string]any{"type": "idle"}}})})
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

// Resume opens a picker connection alongside the TUI owner. That connection
// must relay history without replacing the owner or invalidating its state.
func TestProxyResumePickerConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := newFakeAppServer(t)
	endpoint, client, err := Proxy(ctx, server.wsURL())
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.CloseNow() }()
	upstream := server.acceptConn(t)
	defer func() { _ = upstream.CloseNow() }()
	serverSend(t, owner, envelope{ID: json.RawMessage(`1`), Method: "thread/start"})
	_ = serverReadEnvelope(t, upstream)
	serverSend(t, upstream, envelope{ID: json.RawMessage(`1`), Result: mustJSON(map[string]any{"thread": map[string]any{"id": "owner", "status": map[string]any{"type": "idle"}}})})
	_ = serverReadEnvelope(t, owner)
	<-client.Updates
	picker, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		t.Fatalf("resume picker connection rejected: %v", err)
	}
	pickerUpstream := server.acceptConn(t)
	defer func() { _ = pickerUpstream.CloseNow() }()
	// Reusing an RPC ID on another connection must not bind the owner.
	serverSend(t, picker, envelope{ID: json.RawMessage(`1`), Method: "thread/list"})
	_ = serverReadEnvelope(t, pickerUpstream)
	serverSend(t, pickerUpstream, envelope{ID: json.RawMessage(`1`), Result: mustJSON(map[string]any{"data": []any{}})})
	_ = serverReadEnvelope(t, picker)
	_ = picker.CloseNow()
	serverSend(t, upstream, notif(methodThreadStatusChanged, map[string]any{"threadId": "owner", "status": map[string]any{"type": "active", "activeFlags": []string{}}}))
	_ = serverReadEnvelope(t, owner)
	select {
	case update := <-client.Updates:
		if update.State != bridge.InteractionWorking {
			t.Fatalf("picker changed owner state: %+v", update)
		}
	case <-ctx.Done():
		t.Fatal("owner updates stopped")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed || client.state.threadID != "owner" {
		t.Fatal("picker replaced or closed owner")
	}
}

func TestProxyResumeTransfersResponseOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := newFakeAppServer(t)
	endpoint, client, err := Proxy(ctx, server.wsURL())
	if err != nil {
		t.Fatal(err)
	}
	connect := func(method, thread string) (*websocket.Conn, *websocket.Conn) {
		t.Helper()
		tui, _, err := websocket.Dial(ctx, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		upstream := server.acceptConn(t)
		t.Cleanup(func() { _ = tui.CloseNow(); _ = upstream.CloseNow() })
		serverSend(t, tui, envelope{ID: json.RawMessage(`1`), Method: method})
		_ = serverReadEnvelope(t, upstream)
		serverSend(t, upstream, envelope{ID: json.RawMessage(`1`), Result: mustJSON(map[string]any{"thread": map[string]any{"id": thread, "status": map[string]any{"type": "idle"}}})})
		_ = serverReadEnvelope(t, tui)
		<-client.Updates
		return tui, upstream
	}
	old, oldUpstream := connect("thread/start", "original")
	resumed, upstream := connect("thread/resume", "resumed")
	// Old-connection notifications must not change the resumed owner's state.
	serverSend(t, oldUpstream, notif(methodThreadStatusChanged, map[string]any{"threadId": "resumed", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}}}))
	_ = serverReadEnvelope(t, old)
	_ = old.CloseNow()
	forward := func(env envelope) { t.Helper(); serverSend(t, upstream, env); _ = serverReadEnvelope(t, resumed) }
	forward(notif(methodThreadStatusChanged, map[string]any{"threadId": "resumed", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}}}))
	<-client.Updates
	forward(envelope{ID: json.RawMessage(`42`), Method: methodItemCommandExecApproval, Params: mustJSON(map[string]any{"threadId": "resumed", "turnId": "turn", "itemId": "approval", "command": "printf BLUE", "cwd": "/tmp"})})
	update := <-client.Updates
	if update.Pending == nil {
		t.Fatalf("lost resumed approval: %+v", update)
	}
	if err := client.Decide(ctx, update.Pending.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	response := serverReadEnvelope(t, upstream)
	if string(response.ID) != "42" || string(response.Result) != `{"decision":"accept"}` {
		t.Fatalf("response went to wrong owner: %+v", response)
	}
	_ = resumed.CloseNow()
	select {
	case update := <-client.Updates:
		if update.State != bridge.InteractionUnknown {
			t.Fatalf("owner disconnect: %+v", update)
		}
	case <-ctx.Done():
		t.Fatal("owner disconnect not observed")
	}
	// A later connection to the same endpoint can become owner again.
	connect("thread/resume", "resumed")
}
