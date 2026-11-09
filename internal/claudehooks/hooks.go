// Package claudehooks observes Claude Code's structured lifecycle hooks.
// It has no telemetry, enrollment, or remote control-plane dependency.
package claudehooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/orchael/bridgectl/internal/bridge"
)

var capabilities = bridge.InteractionCapabilities{InteractionStateSupported: true, ApprovalStateSupported: true}

func Capabilities() bridge.InteractionCapabilities { return capabilities }

// Event deliberately excludes prompts, tool arguments/results, paths, and text.
// The short-lived helper removes those fields before contacting the observer.
type Event struct {
	SessionID     string `json:"session_id"`
	Name          string `json:"hook_event_name"`
	Tool          string `json:"tool_name,omitempty"`
	ToolID        string `json:"tool_use_id,omitempty"`
	ToolKey       string `json:"tool_key,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	PromptID      string `json:"prompt_id,omitempty"`
	Source        string `json:"source,omitempty"`
	Notification  string `json:"notification_type,omitempty"`
	ElicitationID string `json:"elicitation_id,omitempty"`
	Server        string `json:"mcp_server_name,omitempty"`
}

type clientConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// PermissionURL is set only with Options.RemoteApproval: the helper then
	// waits there for a remote decision on eligible Bash permission requests.
	PermissionURL string `json:"permission_url,omitempty"`
}

// Options configures optional observer behaviour.
type Options struct {
	// RemoteApproval lets Bridge answer Bash permission requests. The hook
	// helper waits for a remote decision while Claude Code shows its own
	// dialog; whichever answers first wins (Claude kills the hook when the
	// person answers locally). Off by default.
	RemoteApproval bool
}

const (
	// remoteHookTimeoutSeconds is the PermissionRequest hook timeout in
	// remote-approval mode; the helper gives up a little earlier.
	remoteHookTimeoutSeconds = 300
	remoteWaitLimit          = 285 * time.Second
)

type Observer struct {
	SettingsPath string
	ConfigPath   string
	Updates      <-chan bridge.Interaction
	Done         <-chan struct{}

	mu      *sync.Mutex
	state   *state
	publish func(bridge.Interaction)
}

// Decide delivers a remote approval decision ("accept" or "cancel") to the
// hook helper waiting on pendingID. It fails with ErrPendingRequestMismatch
// when nothing is waiting: remote approval is off, the request was already
// answered (locally or remotely), or it timed out.
func (o *Observer) Decide(pendingID, decision string) error {
	if decision != "accept" && decision != "cancel" {
		return bridge.ErrInvalidResponse
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	ch := o.state.waiters[pendingID]
	if ch == nil {
		return bridge.ErrPendingRequestMismatch
	}
	delete(o.state.waiters, pendingID)
	ch <- decision
	return nil
}

// New creates an authenticated loopback receiver and private, session-local
// settings. Cancellation closes the receiver and removes both temporary files.
func New(ctx context.Context, executable string) (*Observer, error) {
	return NewWithOptions(ctx, executable, Options{})
}

// NewWithOptions is New with optional behaviour such as remote approval.
func NewWithOptions(ctx context.Context, executable string, opts Options) (*Observer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "bridgectl-claude-hooks-")
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = listener.Close()
			_ = os.RemoveAll(dir)
		}
	}()
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	cfg := clientConfig{URL: "http://" + listener.Addr().String() + "/event", Token: hex.EncodeToString(secret[:])}
	if opts.RemoteApproval {
		cfg.PermissionURL = "http://" + listener.Addr().String() + "/permission"
	}
	configPath := filepath.Join(dir, "client.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		return nil, err
	}
	// Command hooks support SessionStart as well as tool and notification events.
	// Quote paths for Claude's default POSIX/Git Bash hook shell.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	command := quote(filepath.ToSlash(executable)) + " session report-claude-hook --config " + quote(filepath.ToSlash(configPath))
	hooks := make(map[string]any)
	for _, name := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "Notification", "Stop", "StopFailure", "SessionEnd", "Elicitation", "ElicitationResult", "SubagentStop"} {
		timeout := 2
		if name == "PermissionRequest" && opts.RemoteApproval {
			timeout = remoteHookTimeoutSeconds
		}
		hooks[name] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": timeout}}}}
	}
	data, err = json.Marshal(map[string]any{"hooks": hooks})
	if err != nil {
		return nil, err
	}
	settingsPath := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settingsPath, data, 0o600); err != nil {
		return nil, err
	}
	updates := make(chan bridge.Interaction, 32)
	done := make(chan struct{})
	s := &state{pending: make(map[string]pending), tools: make(map[string]string), waiters: make(map[string]chan string)}
	var mu sync.Mutex
	// publish never stalls the CLI behind a slow consumer; it keeps the latest state.
	publish := func(next bridge.Interaction) {
		select {
		case updates <- next:
		default:
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- next:
			default:
			}
		}
	}
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+cfg.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /event", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Second))
		if !authorized(w, r) {
			return
		}
		var event Event
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&event); err != nil || event.SessionID == "" || event.Name == "" {
			http.Error(w, "invalid hook event", http.StatusBadRequest)
			return
		}
		mu.Lock()
		if next, emit := s.apply(event); emit {
			publish(next)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	if opts.RemoteApproval {
		mux.HandleFunc("POST /permission", func(w http.ResponseWriter, r *http.Request) {
			if !authorized(w, r) {
				return
			}
			var req permissionRequest
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil || req.Event.SessionID == "" || req.Event.Name != "PermissionRequest" || req.Event.Tool != "Bash" || !validRemoteSummary(req.Summary) {
				http.Error(w, "invalid permission request", http.StatusBadRequest)
				return
			}
			// The request is read; wait for a decision without the short
			// server deadlines. Claude killing the hook closes the connection,
			// which cancels r.Context().
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Time{})
			_ = rc.SetWriteDeadline(time.Now().Add(remoteWaitLimit + 5*time.Second))
			decision := make(chan string, 1)
			mu.Lock()
			next, emit := s.apply(req.Event)
			id := ""
			// Register only for an event apply accepted: a stale session or
			// prompt must never take over the current request's waiter.
			// Identical concurrent calls can correlate to the same request, and
			// a decided request stays pending until the tool finishes. Each
			// pending ID is therefore registered at most once; any later
			// helper falls back to the local dialog.
			if p, ok := s.pending[s.keyFor(req.Event)]; emit && ok && p.request.Type == bridge.PendingRequestApproval && !p.registered {
				id = p.request.ID
				p.registered = true
				p.request.Summary = req.Summary
				s.pending[s.keyFor(req.Event)] = p
				s.waiters[id] = decision
				next, emit = s.current(), true
			}
			if emit {
				publish(next)
			}
			mu.Unlock()
			result := ""
			if id != "" {
				timer := time.NewTimer(remoteWaitLimit)
				select {
				case result = <-decision:
				case <-r.Context().Done():
				case <-timer.C:
				case <-ctx.Done():
				}
				timer.Stop()
				mu.Lock()
				if s.waiters[id] == decision {
					delete(s.waiters, id)
				}
				if result == "cancel" {
					// Our deny interrupts the turn, and Claude fires no Stop or
					// PostToolUse hook for that, so resolve the request here.
					for key, p := range s.pending {
						if p.request.ID == id {
							delete(s.pending, key)
						}
					}
					if len(s.pending) == 0 {
						s.base = bridge.InteractionIdle
					}
				}
				publish(s.current())
				mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"decision": result})
		})
	}
	handler := http.Handler(mux)
	// Write deadlines are per handler: events get 2s, permission waits longer.
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second}
	go func() { _ = server.Serve(listener) }()
	go func() {
		<-ctx.Done()
		_ = server.Close()
		// Updates is intentionally not closed: in-flight handlers may still emit.
		// Consumers use the session context, as with the other provider watchers.
		_ = os.RemoveAll(dir)
		close(done)
	}()
	ok = true
	return &Observer{SettingsPath: settingsPath, ConfigPath: configPath, Updates: updates, Done: done, mu: &mu, state: s, publish: publish}, nil
}

// Report forwards only whitelisted hook metadata to the local session observer.
// It never returns a Claude permission decision or writes anything to stdout.
func Report(ctx context.Context, configPath string, input io.Reader) error {
	return ReportTo(ctx, configPath, input, io.Discard)
}

// ReportTo is Report with a destination for the hook's stdout. Only in
// remote-approval mode, and only for an eligible Bash permission request, does
// it wait for a remote decision and write Claude's PermissionRequest decision
// JSON to out. Otherwise it writes nothing, so Claude's own dialog decides.
func ReportTo(ctx context.Context, configPath string, input io.Reader, out io.Writer) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read hook configuration: %w", err)
	}
	var cfg clientConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return errors.New("invalid hook configuration")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/event" || u.User != nil || cfg.Token == "" {
		return errors.New("hook destination must be an authenticated loopback receiver")
	}
	data, err = io.ReadAll(io.LimitReader(input, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return errors.New("hook input exceeds limit or could not be read")
	}
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return errors.New("invalid hook input")
	}
	// PermissionRequest omits tool_use_id. A per-session keyed fingerprint
	// correlates concurrent calls of the same tool without sending arguments
	// or a dictionary-testable content hash to the observer.
	var inputFields struct {
		ToolInput json.RawMessage `json:"tool_input"`
		Cwd       string          `json:"cwd"`
	}
	if err := json.Unmarshal(data, &inputFields); err != nil {
		return errors.New("invalid hook input")
	}
	event.ToolKey = ""
	if len(inputFields.ToolInput) > 0 {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(inputFields.ToolInput))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return errors.New("invalid tool metadata")
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return err
		}
		mac := hmac.New(sha256.New, []byte(cfg.Token))
		_, _ = mac.Write(canonical)
		event.ToolKey = hex.EncodeToString(mac.Sum(nil))
	}
	if cfg.PermissionURL != "" && event.Name == "PermissionRequest" && event.Tool == "Bash" {
		var bash struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(inputFields.ToolInput, &bash)
		if summary, ok := remoteSummary(inputFields.Cwd, bash.Command); ok {
			return waitForRemoteDecision(ctx, cfg, permissionRequest{Event: event, Summary: summary}, out)
		}
	}
	data, err = json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid hook destination")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	// No proxy or redirects: hook credentials and metadata stay on loopback.
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("local session observer unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("local session observer returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// waitForRemoteDecision registers the permission request with the observer
// and blocks until Bridge decides, the wait limit passes, or Claude kills this
// helper because the person answered locally. Only an explicit decision is
// written to out; anything else leaves Claude's own dialog in charge.
func waitForRemoteDecision(ctx context.Context, cfg clientConfig, req permissionRequest, out io.Writer) error {
	u, err := url.Parse(cfg.PermissionURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/permission" || u.User != nil {
		return errors.New("permission destination must be an authenticated loopback receiver")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, remoteWaitLimit+10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.PermissionURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid permission destination")
	}
	httpReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	httpReq.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil // killed by Claude (answered locally) or timed out: no decision
		}
		return errors.New("local session observer unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("local session observer returned HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&reply); err != nil {
		return errors.New("invalid observer reply")
	}
	var decision map[string]any
	switch reply.Decision {
	case "accept":
		// Approve once: never pass updatedPermissions.
		decision = map[string]any{"behavior": "allow"}
	case "cancel":
		// Matches Bridge's "Reject and stop turn".
		decision = map[string]any{"behavior": "deny", "message": "Rejected from Bridge", "interrupt": true}
	default:
		return nil
	}
	return json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": decision}})
}

// pendingKindForTool classifies a permission request from the structured tool
// name Claude's hook reports — never from tool input or terminal text.
func pendingKindForTool(tool string) bridge.PendingRequestKind {
	switch tool {
	case "Bash":
		return bridge.PendingKindCommand
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return bridge.PendingKindFileChange
	case "AskUserQuestion":
		return bridge.PendingKindQuestion
	case "":
		return bridge.PendingKindOther
	default:
		return bridge.PendingKindTool
	}
}

type pending struct {
	request bridge.PendingRequest
	agent   string
	// registered is set once any hook helper has waited on this request, so
	// its ID is single-use: a later correlated request can never reuse it.
	registered bool
}
type state struct {
	session string
	prompt  string
	base    bridge.InteractionStateValue
	pending map[string]pending
	tools   map[string]string
	// waiters holds the hook helpers currently waiting for a remote decision,
	// by pending request ID (remote-approval mode only).
	waiters map[string]chan string
}

// keyFor returns the pending-map key apply uses for a tool event.
func (s *state) keyFor(e Event) string {
	id := e.ToolID
	if id == "" {
		id = s.tools[e.AgentID+"/"+e.Tool+"/"+e.ToolKey]
	}
	if id == "" {
		id = e.Tool
	}
	return e.AgentID + "/tool/" + id
}

func (s *state) apply(e Event) (bridge.Interaction, bool) {
	if e.Name == "SessionStart" && e.AgentID == "" {
		if s.session != e.SessionID || e.Source != "compact" {
			s.session, s.prompt, s.base = e.SessionID, "", bridge.InteractionIdle
			clear(s.pending)
			clear(s.tools)
		}
	} else {
		if s.session == "" {
			s.session = e.SessionID
		}
		if s.session != e.SessionID {
			return bridge.Interaction{}, false
		}
		if e.Name == "UserPromptSubmit" && e.AgentID == "" {
			s.prompt = e.PromptID
		}
		if e.PromptID != "" && s.prompt != "" && e.PromptID != s.prompt {
			return bridge.Interaction{}, false
		}
		toolKey := e.AgentID + "/" + e.Tool + "/" + e.ToolKey
		if e.ToolID != "" {
			s.tools[toolKey] = e.ToolID
		}
		id := e.ToolID
		if id == "" {
			id = s.tools[toolKey]
		}
		if id == "" {
			id = e.Tool
		}
		key := e.AgentID + "/tool/" + id
		add := func(key string, typ bridge.PendingRequestType, kind bridge.PendingRequestKind) {
			if old, ok := s.pending[key]; ok && old.request.Type == typ {
				return
			}
			s.pending[key] = pending{request: bridge.PendingRequest{ID: uuid.NewString(), Type: typ, Kind: kind}, agent: e.AgentID}
		}
		switch e.Name {
		case "UserPromptSubmit":
			if e.AgentID != "" {
				return bridge.Interaction{}, false
			}
			clear(s.pending)
			clear(s.tools)
			s.base = bridge.InteractionWorking
		case "PreToolUse":
			s.base = bridge.InteractionWorking
			if e.Tool == "AskUserQuestion" {
				add(key, bridge.PendingRequestInput, bridge.PendingKindQuestion)
			}
		case "PermissionRequest":
			typ := bridge.PendingRequestApproval
			if e.Tool == "AskUserQuestion" {
				typ = bridge.PendingRequestInput
			}
			add(key, typ, pendingKindForTool(e.Tool))
		case "PostToolUse", "PostToolUseFailure":
			delete(s.pending, key)
			delete(s.tools, toolKey)
			s.base = bridge.InteractionWorking
		case "Elicitation":
			add(e.AgentID+"/elicitation/"+e.Server+"/"+e.ElicitationID, bridge.PendingRequestInput, bridge.PendingKindQuestion)
		case "ElicitationResult":
			delete(s.pending, e.AgentID+"/elicitation/"+e.Server+"/"+e.ElicitationID)
			s.base = bridge.InteractionWorking
		case "Stop", "StopFailure", "SubagentStop":
			for key, p := range s.pending {
				if p.agent == e.AgentID {
					delete(s.pending, key)
				}
			}
			if e.AgentID == "" {
				s.base = bridge.InteractionIdle
				if e.Name == "StopFailure" {
					s.base = bridge.InteractionUnknown
				}
			}
		case "SessionEnd":
			if e.AgentID != "" {
				return bridge.Interaction{}, false
			}
			clear(s.pending)
			clear(s.tools)
			s.base = bridge.InteractionUnknown
		case "Notification":
			// An idle notification does not mean a specific question is pending.
			// Permission/input notifications lack resolution identity; use the
			// request hooks above instead of creating unresolvable requests.
			if e.Notification != "idle_prompt" || e.AgentID != "" {
				return bridge.Interaction{}, false
			}
			s.base = bridge.InteractionIdle
		default:
			return bridge.Interaction{}, false
		}
	}
	return s.current(), true
}

// current projects the state into an Interaction. Structured approval is
// advertised only while a hook helper is actually waiting for the selected
// request, so it disappears as soon as the request is answered anywhere.
func (s *state) current() bridge.Interaction {
	next := bridge.Interaction{State: s.base, Evidence: bridge.InteractionEvidence{Source: "claude-hooks", Capability: capabilities}}
	// Stable selection across map iterations; approvals take precedence.
	selected := ""
	for key, p := range s.pending {
		if next.Pending == nil || (p.request.Type == bridge.PendingRequestApproval && next.Pending.Type != bridge.PendingRequestApproval) || (p.request.Type == next.Pending.Type && key < selected) {
			request := p.request
			next.Pending = &request
			selected = key
		}
	}
	if next.Pending != nil {
		next.State = bridge.InteractionWaitingForInput
		if next.Pending.Type == bridge.PendingRequestApproval {
			next.State = bridge.InteractionWaitingForApproval
			if s.waiters[next.Pending.ID] != nil {
				next.Evidence.Capability.StructuredApprovalSupported = true
				next.Evidence.Capability.PendingSummarySupported = next.Pending.Summary != ""
			}
		}
	}
	return next
}

type permissionRequest struct {
	Event   Event  `json:"event"`
	Summary string `json:"summary"`
}

// remoteSummary builds the approval summary for a Bash permission request,
// matching Codex's actionable wording. ok is false when the request must stay
// observe-only (no cwd or command, control characters, or over the cap).
func remoteSummary(cwd, command string) (string, bool) {
	if strings.TrimSpace(cwd) == "" || strings.TrimSpace(command) == "" {
		return "", false
	}
	summary := "Run once in " + cwd + ": " + command
	return summary, validRemoteSummary(summary)
}

// validRemoteSummary admits no control characters at all (not even the
// newline and tab bridge.ValidateResponse allows): a multi-line command could
// hide what is really being approved.
func validRemoteSummary(summary string) bool {
	if summary == "" || len(summary) > remoteSummaryCap || !utf8.ValidString(summary) {
		return false
	}
	for _, r := range summary {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// remoteSummaryCap matches Bridge's pending_request_summary column limit.
const remoteSummaryCap = 200
