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
}

type Observer struct {
	SettingsPath string
	ConfigPath   string
	Updates      <-chan bridge.Interaction
	Done         <-chan struct{}
}

// New creates an authenticated loopback receiver and private, session-local
// settings. Cancellation closes the receiver and removes both temporary files.
func New(ctx context.Context, executable string) (*Observer, error) {
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
		hooks[name] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 2}}}}
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
	s := &state{pending: make(map[string]pending), tools: make(map[string]string)}
	var mu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/event" {
			http.NotFound(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+cfg.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var event Event
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&event); err != nil || event.SessionID == "" || event.Name == "" {
			http.Error(w, "invalid hook event", http.StatusBadRequest)
			return
		}
		mu.Lock()
		if next, emit := s.apply(event); emit {
			// Never stall the CLI behind a slow observer. Preserve the latest state.
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
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
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
	return &Observer{SettingsPath: settingsPath, ConfigPath: configPath, Updates: updates, Done: done}, nil
}

// Report forwards only whitelisted hook metadata to the local session observer.
// It never returns a Claude permission decision or writes anything to stdout.
func Report(ctx context.Context, configPath string, input io.Reader) error {
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
}
type state struct {
	session string
	prompt  string
	base    bridge.InteractionStateValue
	pending map[string]pending
	tools   map[string]string
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
		}
	}
	return next, true
}
