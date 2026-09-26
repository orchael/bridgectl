package provider

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/orchael/bridgectl/internal/bridge"
)

func TestCodexObserverPreservesLaunchArguments(t *testing.T) {
	root := t.TempDir()
	p := NewCodexAppServerProvider(StdioConfig{ProviderID: "codex", Binary: "/usr/bin/node", ProviderRoot: root, DefaultArgs: []string{"./codex.js", "--model", "test-model", "-c", "features.example=true", "--no-alt-screen"}})
	app, tui, err := p.launchCommands(context.Background(), bridge.SessionConfig{Env: []string{"HOME=" + root, "OPENAI_API_KEY=test-only"}}, "ws://127.0.0.1:12345")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/usr/bin/node", filepath.Join(root, "codex.js"), "--model", "test-model", "-c", "features.example=true", "--no-alt-screen"}; !reflect.DeepEqual(tui.Args, want) {
		t.Fatalf("TUI args=%v, want %v", tui.Args, want)
	}
	if want := []string{"/usr/bin/node", filepath.Join(root, "codex.js"), "app-server", "--listen", "ws://127.0.0.1:12345", "-c", "features.example=true"}; !reflect.DeepEqual(app.Args, want) {
		t.Fatalf("app args=%v, want %v", app.Args, want)
	}
}
