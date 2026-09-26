package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
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
	threadRequests := make(map[string]bool)
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
				var request envelope
				if json.Unmarshal(b, &request) == nil && len(request.ID) > 0 && (request.Method == "thread/start" || request.Method == "thread/resume" || request.Method == "thread/fork") {
					client.mu.Lock()
					threadRequests[string(request.ID)] = true
					slog.Debug("codexapp owner request", "method", request.Method, "rpc_id", string(request.ID))
					client.mu.Unlock()
				}
				client.mu.Lock()
				if request.Method == "" && len(request.ID) > 0 && string(request.ID) == string(client.requestID) {
					client.submitted = true
				}
				client.mu.Unlock()
				err = conn.Write(relayCtx, typ, b)
				if err != nil {
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
				// Internal instruction responses never leak into the TUI's RPC namespace.
				if env.Method == "" && strings.HasPrefix(string(env.ID), `"bridgectl-`) {
					if ch := client.instructionResults[string(env.ID)]; ch != nil {
						var resultErr error
						if env.Error != nil {
							resultErr = bridge.ErrPendingRequestMismatch
						}
						select {
						case ch <- resultErr:
						default:
						}
					}
					client.mu.Unlock()
					continue
				}
				client.observeActivityLocked(env)
				// The TUI reads historical threads during startup. Only a
				// response to its own non-ephemeral start/resume/fork operation establishes
				// session ownership; an unrelated thread/read result must not.
				if env.Method == "" && threadRequests[string(env.ID)] {
					delete(threadRequests, string(env.ID))
					var result threadStartedParams
					if json.Unmarshal(env.Result, &result) == nil && result.Thread.ID != "" && !result.Thread.Ephemeral {
						slog.Debug("codexapp owner binding", "rpc_id", string(env.ID), "thread_id", result.Thread.ID, "status", result.Thread.Status.Type)
						if client.state.threadID != result.Thread.ID {
							client.state = observerState{threadID: result.Thread.ID}
							client.turnID = ""
							client.requestID = nil
							client.questionID = ""
							client.approval = false
							client.submitted = false
						}
						env.Method = methodThreadStarted
						env.Params = env.Result
					}
				}
				if env.Method == methodThreadStatusChanged || env.Method == methodItemToolRequestUserInput {
					var meta struct {
						ThreadID string              `json:"threadId"`
						Status   threadStatusPayload `json:"status"`
					}
					_ = json.Unmarshal(env.Params, &meta)
					slog.Debug("codexapp thread evidence", "method", env.Method, "thread_id", meta.ThreadID, "owner_thread", client.state.threadID, "status", meta.Status)
				}
				if env.Method == "item/completed" {
					var completion struct {
						ThreadID string `json:"threadId"`
						Item     struct {
							Type     string `json:"type"`
							ID       string `json:"id"`
							Status   string `json:"status"`
							ExitCode *int   `json:"exitCode"`
						} `json:"item"`
					}
					if json.Unmarshal(env.Params, &completion) == nil && completion.ThreadID == client.state.threadID && completion.Item.Type == "commandExecution" {
						var exitCode any
						if completion.Item.ExitCode != nil {
							exitCode = *completion.Item.ExitCode
						}
						slog.Debug("codexapp command completion", "thread_id", completion.ThreadID, "item_id", completion.Item.ID, "status", completion.Item.Status, "exit_code", exitCode)
					}
				}
				var interaction bridge.Interaction
				var emit bool
				if client.state.threadID != "" {
					interaction, emit = handleMessage(&client.state, env, slog.Default())
				}
				if env.Method == methodItemToolRequestUserInput {
					var p toolRequestUserInputParams
					if json.Unmarshal(env.Params, &p) == nil && p.ThreadID == client.state.threadID && string(client.requestID) != string(env.ID) {
						client.questionID = ""
						client.approval = false
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
				if env.Method == methodItemCommandExecApproval || env.Method == methodItemFileChangeApproval || env.Method == methodItemPermissionsApproval {
					var p commandApprovalParams
					if json.Unmarshal(env.Params, &p) == nil && p.ThreadID == client.state.threadID {
						if string(client.requestID) != string(env.ID) {
							client.submitted = false
						}
						client.requestID = append(json.RawMessage(nil), env.ID...)
						client.questionID = ""
						client.approval = approvalSupported(env, p)
						slog.Debug("codexapp approval capability", "method", env.Method, "supported", client.approval, "command_bytes", len(p.Command), "cwd_bytes", len(p.Cwd), "turn_present", p.TurnID != "", "network_present", len(p.Network) > 0 && string(p.Network) != "null", "permissions_present", len(p.Permissions) > 0 && string(p.Permissions) != "null", "decision_count", len(p.AvailableDecisions))
					}
				}
				if env.Method == "serverRequest/resolved" {
					var p struct {
						ThreadID  string          `json:"threadId"`
						RequestID json.RawMessage `json:"requestId"`
					}
					if json.Unmarshal(env.Params, &p) == nil && p.ThreadID == client.state.threadID && string(p.RequestID) == string(client.requestID) {
						client.questionID = ""
						client.approval = false
						client.requestID = nil
						client.submitted = true
					}
				}
				if emit {
					if interaction.State != client.lastActivityState {
						client.lastActivityState = interaction.State
						label := map[bridge.InteractionStateValue]string{bridge.InteractionWorking: "Working", bridge.InteractionIdle: "Idle", bridge.InteractionWaitingForInput: "Waiting for your answer", bridge.InteractionWaitingForApproval: "Waiting for approval", bridge.InteractionUnknown: "Interaction state unknown"}[interaction.State]
						client.activity.Append("status", label, time.Now())
					}
					interaction.Evidence.Capability.StructuredApprovalSupported = interaction.State == bridge.InteractionWaitingForApproval && interaction.Pending != nil && client.approval && !client.submitted && len(client.requestID) > 0
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
