package claudehooks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

// Session reporting must work locally without enrollment or telemetry.
func TestHookReportingLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o, err := New(ctx, "/path with spaces/bridgectl")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		event, tool, id string
		state           bridge.InteractionStateValue
	}{
		{"SessionStart", "", "", bridge.InteractionIdle},
		{"UserPromptSubmit", "", "", bridge.InteractionWorking},
		{"PreToolUse", "AskUserQuestion", "question-1", bridge.InteractionWaitingForInput},
		{"PostToolUse", "AskUserQuestion", "question-1", bridge.InteractionWorking},
		{"PermissionRequest", "Bash", "tool-2", bridge.InteractionWaitingForApproval},
		{"PostToolUseFailure", "Bash", "tool-2", bridge.InteractionWorking},
		{"Stop", "", "", bridge.InteractionIdle},
		{"SessionEnd", "", "", bridge.InteractionUnknown},
	} {
		payload, _ := json.Marshal(map[string]string{"session_id": "native-session", "hook_event_name": step.event, "tool_name": step.tool, "tool_use_id": step.id, "prompt": "private prompt", "last_assistant_message": "private answer"})
		if err := Report(ctx, o.ConfigPath, strings.NewReader(string(payload))); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-o.Updates:
			if got.State != step.state {
				t.Fatalf("%s: state = %s, want %s", step.event, got.State, step.state)
			}
			if got.Evidence.Source != "claude-hooks" || !got.Evidence.Capability.InteractionStateSupported {
				t.Fatalf("missing evidence: %+v", got)
			}
			if got.Evidence.Capability.RemoteResponseSupported || got.Evidence.Capability.StructuredApprovalSupported {
				t.Fatal("observer advertised response capability")
			}
			if step.state == bridge.InteractionWaitingForInput || step.state == bridge.InteractionWaitingForApproval {
				if got.Pending == nil || got.Pending.ID == "" || got.Pending.Summary != "" {
					t.Fatalf("unsafe or missing pending identity: %+v", got.Pending)
				}
			} else if got.Pending != nil {
				t.Fatalf("stale request: %+v", got.Pending)
			}
		case <-time.After(time.Second):
			t.Fatalf("no update for %s", step.event)
		}
	}
	settings, err := os.ReadFile(o.SettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(settings), "private") || !strings.Contains(string(settings), "report-claude-hook") {
		t.Fatalf("invalid settings")
	}
	cancel()
	select {
	case <-o.Done:
	case <-time.After(time.Second):
		t.Fatal("observer did not stop")
	}
	if _, err := os.Stat(o.SettingsPath); !os.IsNotExist(err) {
		t.Fatalf("settings not cleaned up: %v", err)
	}
}

func TestConcurrentRequestsAndStaleEvents(t *testing.T) {
	s := &state{pending: map[string]pending{}, tools: map[string]string{}}
	apply := func(name, tool, id, agent string) bridge.Interaction {
		t.Helper()
		got, ok := s.apply(Event{SessionID: "s", Name: name, Tool: tool, ToolID: id, AgentID: agent})
		if !ok {
			t.Fatalf("ignored %s", name)
		}
		return got
	}
	apply("SessionStart", "", "", "")
	apply("PreToolUse", "Bash", "one", "")
	// Claude's PermissionRequest omits tool_use_id; correlate the preceding call.
	first := apply("PermissionRequest", "Bash", "", "")
	if repeated := apply("PermissionRequest", "Bash", "", ""); repeated.Pending.ID != first.Pending.ID {
		t.Fatal("request identity changed on duplicate")
	}
	apply("PreToolUse", "Read", "two", "child")
	if got := apply("PostToolUse", "Read", "two", "child"); got.State != bridge.InteractionWaitingForApproval {
		t.Fatal("unrelated subagent cleared approval")
	}
	if got := apply("SubagentStop", "", "", "child"); got.Pending.ID != first.Pending.ID {
		t.Fatal("subagent stop cleared parent request")
	}
	if got := apply("PostToolUseFailure", "Bash", "one", ""); got.State != bridge.InteractionWorking || got.Pending != nil {
		t.Fatal("denial did not clear request")
	}
	_, _ = s.apply(Event{SessionID: "s", Name: "UserPromptSubmit", PromptID: "new"})
	if _, ok := s.apply(Event{SessionID: "s", Name: "Stop", PromptID: "old"}); ok {
		t.Fatal("old turn overwrote current state")
	}
	if _, ok := s.apply(Event{SessionID: "other", Name: "Stop"}); ok {
		t.Fatal("another session overwrote current state")
	}
	if got := apply("Stop", "", "", ""); got.State != bridge.InteractionIdle {
		t.Fatal("completed turn is not idle")
	}
	if got, ok := s.apply(Event{SessionID: "s", Name: "Notification", Notification: "idle_prompt"}); !ok || got.Pending != nil || got.State != bridge.InteractionIdle {
		t.Fatal("idle notification fabricated input request")
	}
}

func TestElicitationAndResume(t *testing.T) {
	s := &state{pending: map[string]pending{}, tools: map[string]string{}}
	_, _ = s.apply(Event{SessionID: "s", Name: "SessionStart"})
	got, _ := s.apply(Event{SessionID: "s", Name: "Elicitation", Server: "private-server", ElicitationID: "e1"})
	if got.State != bridge.InteractionWaitingForInput || strings.Contains(got.Pending.ID, "private-server") {
		t.Fatalf("bad elicitation: %+v", got)
	}
	got, _ = s.apply(Event{SessionID: "s", Name: "ElicitationResult", Server: "private-server", ElicitationID: "e1"})
	if got.Pending != nil || got.State != bridge.InteractionWorking {
		t.Fatal("elicitation unresolved")
	}
	got, _ = s.apply(Event{SessionID: "resumed", Name: "SessionStart", Source: "resume"})
	if got.Pending != nil || got.State != bridge.InteractionIdle {
		t.Fatal("resume retained old state")
	}
	if _, ok := s.apply(Event{SessionID: "s", Name: "PermissionRequest", Tool: "Bash"}); ok {
		t.Fatal("previous native session overwrote resumed state")
	}
}

func TestParallelToolsCorrelatePermissionWithoutToolID(t *testing.T) {
	s := &state{pending: map[string]pending{}, tools: map[string]string{}}
	_, _ = s.apply(Event{SessionID: "s", Name: "SessionStart"})
	_, _ = s.apply(Event{SessionID: "s", Name: "PreToolUse", Tool: "Bash", ToolID: "first", ToolKey: "first-input"})
	_, _ = s.apply(Event{SessionID: "s", Name: "PreToolUse", Tool: "Bash", ToolID: "second", ToolKey: "second-input"})
	_, _ = s.apply(Event{SessionID: "s", Name: "PermissionRequest", Tool: "Bash", ToolKey: "first-input"})
	got, _ := s.apply(Event{SessionID: "s", Name: "PostToolUse", Tool: "Bash", ToolID: "second", ToolKey: "second-input"})
	if got.State != bridge.InteractionWaitingForApproval {
		t.Fatal("unrelated Bash completion cleared first approval")
	}
	got, _ = s.apply(Event{SessionID: "s", Name: "PostToolUseFailure", Tool: "Bash", ToolID: "first", ToolKey: "first-input"})
	if got.Pending != nil || got.State != bridge.InteractionWorking {
		t.Fatal("matching completion left approval pending")
	}
}

func TestHookTransportPrivacyAndAuthentication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o, err := New(ctx, "bridgectl")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(o.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg clientConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{o.ConfigPath, o.SettingsPath} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unsafe permissions: %v", err)
		}
	}
	resp, err := http.Post(cfg.URL, "application/json", strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("unauthenticated hook accepted")
	}
	select {
	case <-o.Updates:
		t.Fatal("unauthenticated hook changed state")
	default:
	}
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "client.json")
	data, _ = json.Marshal(clientConfig{URL: server.URL + "/event", Token: "test-only"})
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Report(ctx, configPath, strings.NewReader(`{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"private command"},"transcript_path":"/private/path","prompt":"private prompt"}`)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(received, "private") || strings.Contains(received, "tool_input") {
		t.Fatal("private hook content left helper")
	}
	if err := Report(ctx, configPath, strings.NewReader(`{invalid`)); err == nil {
		t.Fatal("malformed hook accepted")
	}
	data, _ = json.Marshal(clientConfig{URL: "https://example.com/event", Token: "test-only"})
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Report(ctx, configPath, strings.NewReader(`{}`)); err == nil {
		t.Fatal("nonlocal receiver accepted")
	}
}

// TestPendingKindIsClassifiedFromToolName covers the fixed-vocabulary kind
// derived from the structured tool name, never from tool input or text.
func TestPendingKindIsClassifiedFromToolName(t *testing.T) {
	cases := map[string]bridge.PendingRequestKind{
		"Bash": bridge.PendingKindCommand, "Edit": bridge.PendingKindFileChange, "Write": bridge.PendingKindFileChange,
		"AskUserQuestion": bridge.PendingKindQuestion, "WebFetch": bridge.PendingKindTool, "": bridge.PendingKindOther,
	}
	for tool, want := range cases {
		s := &state{pending: map[string]pending{}, tools: map[string]string{}}
		if _, ok := s.apply(Event{SessionID: "s", Name: "SessionStart"}); !ok {
			t.Fatal("SessionStart ignored")
		}
		got, ok := s.apply(Event{SessionID: "s", Name: "PermissionRequest", Tool: tool})
		if !ok || got.Pending == nil {
			t.Fatalf("%q: no pending request", tool)
		}
		if got.Pending.Kind != want {
			t.Errorf("tool %q: kind = %q, want %q", tool, got.Pending.Kind, want)
		}
	}
}
