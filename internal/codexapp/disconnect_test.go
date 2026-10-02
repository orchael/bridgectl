package codexapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/orchael/bridgectl/internal/bridge"
)

func TestProxyDisconnectInvalidatesInteraction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	serverSend(t, tui, envelope{ID: json.RawMessage(`1`), Method: "thread/start"})
	serverReadEnvelope(t, upstream)
	serverSend(t, upstream, envelope{ID: json.RawMessage(`1`), Result: mustJSON(map[string]any{"thread": map[string]any{"id": "owner", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnUserInput"}}}})})
	serverReadEnvelope(t, tui)
	if got := <-client.Updates; got.State != bridge.InteractionWaitingForInput {
		t.Fatalf("initial state=%s", got.State)
	}
	_ = upstream.CloseNow()
	select {
	case got := <-client.Updates:
		if got.State != bridge.InteractionUnknown || got.Pending != nil || got.Evidence.Capability.RemoteResponseSupported {
			t.Fatalf("stale state after disconnect: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("disconnect left waiting state active")
	}
}
