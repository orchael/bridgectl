package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/claudehooks"
)

func TestClaudeProviderPreservesSettingsAndCleansUp(t *testing.T) {
	repo := t.TempDir()
	original := `{"model":"test-model","permissions":{"defaultMode":"plan"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo existing-hook"}]}]}}`
	userPath := filepath.Join(repo, "custom.json")
	if err := os.WriteFile(userPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewClaudeHooksProvider(StdioConfig{ProviderID: "claude", Binary: "/bin/cat", DefaultArgs: []string{"--settings", "custom.json", "--verbose"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, err := p.BuildCommand(ctx, bridge.SessionConfig{SessionID: "s", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := cmd.Args[len(cmd.Args)-1]
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if string(settings["model"]) != `"test-model"` || !strings.Contains(string(data), "existing-hook") || !strings.Contains(string(data), "report-claude-hook") {
		t.Fatal("lost user settings/hooks")
	}
	if data, err := os.ReadFile(userPath); err != nil || string(data) != original {
		t.Fatal("changed user's settings file")
	}
	if _, err := p.WatchInteraction(ctx, "s", bridge.SessionConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WatchInteraction(ctx, "missing", bridge.SessionConfig{}); err == nil {
		t.Fatal("missing observer accepted")
	}
	p.mu.Lock()
	o := p.sessions["s"]
	p.mu.Unlock()
	cancel()
	select {
	case <-o.Done:
	case <-time.After(time.Second):
		t.Fatal("observer not cleaned up")
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatal("private settings retained")
	}
}

func TestClaudeSettingsBeforePositionalDelimiter(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := mergeClaudeSettings([]string{"claude", "--verbose", "--", "--settings=this is prompt text"}, settings, ".")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--verbose", "--settings", settings, "--", "--settings=this is prompt text"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%v, want %v", got, want)
	}
}

var _ bridge.PendingApprovalResponder = (*ClaudeHooksProvider)(nil)

// Remote approval is opt-in: the env flag switches the generated
// PermissionRequest hook to a long timeout; without it nothing changes, and a
// decision for an unknown session or request is a mismatch.
func TestClaudeRemoteApprovalOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		if enabled {
			t.Setenv(ClaudeRemoteApprovalEnv, "1")
		}
		p := NewClaudeHooksProvider(StdioConfig{ProviderID: "claude", Binary: "/bin/cat"})
		ctx, cancel := context.WithCancel(context.Background())
		cmd, err := p.BuildCommand(ctx, bridge.SessionConfig{SessionID: "s", RepoPath: t.TempDir()})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		data, err := os.ReadFile(cmd.Args[len(cmd.Args)-1])
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		long := strings.Contains(string(data), `"timeout":300`)
		if long != enabled {
			cancel()
			t.Fatalf("enabled=%v but long PermissionRequest timeout=%v", enabled, long)
		}
		if err := p.DecideApproval(ctx, "s", "nothing-waiting", "accept"); !errors.Is(err, bridge.ErrPendingRequestMismatch) {
			cancel()
			t.Fatalf("decision without a waiting request: %v", err)
		}
		if err := p.DecideApproval(ctx, "missing", "x", "accept"); !errors.Is(err, bridge.ErrPendingRequestMismatch) {
			cancel()
			t.Fatalf("decision for unknown session: %v", err)
		}
		cancel()
	}
}

// Bosun review: exercise the provider's DecideApproval path end to end with a
// waiting hook helper, not just Observer.Decide. The Supervisor gate that lets
// approvals through while a terminal writer is attached is covered by
// bridge.TestMAR66ApprovalOwnership/writer.
func TestClaudeRemoteApprovalThroughProvider(t *testing.T) {
	t.Setenv(ClaudeRemoteApprovalEnv, "1")
	p := NewClaudeHooksProvider(StdioConfig{ProviderID: "claude", Binary: "/bin/cat"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := p.BuildCommand(ctx, bridge.SessionConfig{SessionID: "s", RepoPath: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	updates, err := p.WatchInteraction(ctx, "s", bridge.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	configPath := p.sessions["s"].ConfigPath
	p.mu.Unlock()
	input := `{"session_id":"n","hook_event_name":"PermissionRequest","tool_name":"Bash","cwd":"/repo","tool_input":{"command":"git push"}}`
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- claudehooks.ReportTo(ctx, configPath, strings.NewReader(input), &out) }()
	var pendingID string
	select {
	case u := <-updates:
		if !u.Evidence.Capability.StructuredApprovalSupported || u.Pending == nil {
			t.Fatalf("not actionable: %+v", u)
		}
		pendingID = u.Pending.ID
	case <-time.After(2 * time.Second):
		t.Fatal("no update")
	}
	if err := p.DecideApproval(ctx, "s", pendingID, "accept"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"behavior":"allow"`) {
		t.Fatalf("hook output %q", out.String())
	}
}
