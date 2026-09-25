package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/orchael/bridgectl/internal/bridge"
)

// Proxy keeps the real TUI as the protocol owner. Codex sends pending
// requests only to that owner, not to a second status observer. Forwarding
// its connection gives responses the original RPC request identity and
// lets the TUI receive the provider's subsequent resolution notification.
// Only one local TUI connection is accepted for this session lifetime.
func Proxy(ctx context.Context, upstream string) (string, *ResponseClient, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	updates := make(chan bridge.Interaction, 64)
	client := &ResponseClient{Updates: updates}
	taken := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client.mu.Lock()
		if taken {
			client.mu.Unlock()
			http.Error(w, "session already attached", http.StatusConflict)
			return
		}
		taken = true
		client.mu.Unlock()
		tui, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = tui.CloseNow() }()
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, _, err := websocket.Dial(dialCtx, upstream, nil)
		cancel()
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		conn.SetReadLimit(16 << 20)
		tui.SetReadLimit(16 << 20)
		client.mu.Lock()
		client.conn = conn
		client.mu.Unlock()
		defer func() { client.mu.Lock(); client.closed = true; client.mu.Unlock() }()
		relayCtx, stop := context.WithCancel(ctx)
		defer stop()
		go func() {
			defer stop()
			for {
				typ, b, err := tui.Read(relayCtx)
				if err != nil {
					return
				}
				if err = conn.Write(relayCtx, typ, b); err != nil {
					return
				}
			}
		}()
		for {
			typ, b, err := conn.Read(relayCtx)
			if err != nil {
				return
			}
			var env envelope
			if json.Unmarshal(b, &env) == nil {
				client.mu.Lock()
				if env.Method == "" && len(env.Result) > 0 {
					var result threadStartedParams
					if json.Unmarshal(env.Result, &result) == nil && result.Thread.ID != "" {
						env.Method = methodThreadStarted
						env.Params = env.Result
					}
				}
				interaction, emit := handleMessage(&client.state, env, slog.Default())
				if env.Method == methodItemToolRequestUserInput {
					var p toolRequestUserInputParams
					if json.Unmarshal(env.Params, &p) == nil && p.ThreadID == client.state.threadID && string(client.requestID) != string(env.ID) {
						client.questionID = ""
						client.requestID = nil
						client.submitted = false
					}
					if json.Unmarshal(env.Params, &p) == nil && p.ThreadID == client.state.threadID && len(p.Questions) == 1 && p.Questions[0].ID != "" && !p.Questions[0].IsSecret && len(env.ID) > 0 {
						if string(client.requestID) != string(env.ID) {
							client.submitted = false
						}
						client.requestID = append(json.RawMessage(nil), env.ID...)
						client.questionID = p.Questions[0].ID
					}
				}
				if env.Method == "serverRequest/resolved" {
					var p struct {
						ThreadID  string          `json:"threadId"`
						RequestID json.RawMessage `json:"requestId"`
					}
					if json.Unmarshal(env.Params, &p) == nil && p.ThreadID == client.state.threadID && string(p.RequestID) == string(client.requestID) {
						client.questionID = ""
						client.requestID = nil
						client.submitted = true
					}
				}
				if emit {
					interaction.Evidence.Capability.RemoteResponseSupported = interaction.State == bridge.InteractionWaitingForInput && interaction.Pending != nil && client.questionID != "" && len(client.requestID) > 0 && !client.submitted
					// Never block the TUI on Bridge consumption. A saturated watcher
					// keeps the latest authoritative state instead of terminal traffic.
					select {
					case updates <- interaction:
					default:
						select {
						case <-updates:
						default:
						}
						select {
						case updates <- interaction:
						default:
						}
					}
				}
				client.mu.Unlock()
			}
			if err := tui.Write(relayCtx, typ, b); err != nil {
				return
			}
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	go func() { <-ctx.Done(); _ = server.Close() }()
	return fmt.Sprintf("ws://%s", listener.Addr()), client, nil
}
