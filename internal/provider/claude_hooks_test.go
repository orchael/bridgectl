package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
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
