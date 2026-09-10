package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orchael/bridgectl/internal/bridge"
)

func newTestCodexProvider() *CodexProvider {
	return NewCodexProvider(StdioConfig{
		ProviderID:   "codex",
		Binary:       "/bin/echo",
		StartupProbe: "none",
	})
}

func clearCodexEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_AUTH", "CODEX_HOME"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv("HOME", t.TempDir())
}

func TestCodexValidateStartup_NoAuth(t *testing.T) {
	clearCodexEnv(t)
	p := newTestCodexProvider()

	err := p.ValidateStartup(context.Background())
	if err == nil {
		t.Fatal("expected error when no auth env is set")
	}
	if !strings.Contains(err.Error(), "CODEX_AUTH") {
		t.Fatalf("error should mention CODEX_AUTH, got: %v", err)
	}
}

func TestCodexValidateStartup_OpenAIKey(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	p := newTestCodexProvider()

	if err := p.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("unexpected error with OPENAI_API_KEY: %v", err)
	}
}

func TestCodexValidateStartup_CodexAPIKey(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("CODEX_API_KEY", "sk-test")
	p := newTestCodexProvider()

	if err := p.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("unexpected error with CODEX_API_KEY: %v", err)
	}
}

func TestCodexValidateStartup_CodexAuth(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("CODEX_AUTH", lifecycleSeed)
	p := newTestCodexProvider()

	if err := p.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("unexpected error with CODEX_AUTH: %v", err)
	}
}

func TestCodexValidateStartup_CodexHomeAuthFile(t *testing.T) {
	clearCodexEnv(t)
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(lifecycleSeed), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	p := newTestCodexProvider()

	if err := p.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("unexpected error with CODEX_HOME auth.json: %v", err)
	}
}

func TestCodexValidateStartup_DefaultHomeAuthFile(t *testing.T) {
	clearCodexEnv(t)
	codexHome := filepath.Join(os.Getenv("HOME"), ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(lifecycleSeed), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	p := newTestCodexProvider()

	if err := p.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("unexpected error with default auth.json: %v", err)
	}
}

func TestCodexHealth_NoAuth(t *testing.T) {
	clearCodexEnv(t)
	p := newTestCodexProvider()

	err := p.Health(context.Background())
	if err == nil {
		t.Fatal("expected error when no auth env is set")
	}
}

func TestCodexHealth_WithAPIKey(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	p := newTestCodexProvider()

	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCodexHealth_WithCodexAuth(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("CODEX_AUTH", lifecycleSeed)
	p := newTestCodexProvider()

	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCodexBuildCommand_WithCodexAuth(t *testing.T) {
	clearCodexEnv(t)
	authJSON := lifecycleSeed
	t.Setenv("CODEX_AUTH", authJSON)

	p := newTestCodexProvider()
	t.Cleanup(p.Cleanup)

	cfg := bridge.SessionConfig{
		ProjectID: "test",
		SessionID: "s1",
		RepoPath:  t.TempDir(),
	}
	cmd, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	// Verify CODEX_HOME is set in the subprocess env.
	var codexHome string
	for _, e := range cmd.Env {
		if v, ok := strings.CutPrefix(e, "CODEX_HOME="); ok {
			codexHome = v
		}
	}
	if codexHome == "" {
		t.Fatal("CODEX_HOME not set in subprocess env")
	}
	wantCodexHome := filepath.Join(os.Getenv("HOME"), ".config/bridgectl", "codex-home")
	if codexHome != wantCodexHome {
		t.Fatalf("CODEX_HOME=%q want %q", codexHome, wantCodexHome)
	}

	// Verify auth.json was written with correct contents.
	data, err := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if string(data) != authJSON {
		t.Fatalf("auth.json content = %q, want %q", string(data), authJSON)
	}

	// Verify file permissions are restrictive.
	info, err := os.Stat(filepath.Join(codexHome, "auth.json"))
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("auth.json perms = %o, want 0600", perm)
	}
}

func TestCodexBuildCommand_WithCodexAuthRespectsExplicitCodexHome(t *testing.T) {
	clearCodexEnv(t)
	authJSON := lifecycleSeed
	explicitCodexHome := filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_AUTH", authJSON)
	t.Setenv("CODEX_HOME", explicitCodexHome)

	p := newTestCodexProvider()
	t.Cleanup(p.Cleanup)

	cfg := bridge.SessionConfig{
		ProjectID: "test",
		SessionID: "s1",
		RepoPath:  t.TempDir(),
	}
	cmd, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	if got := envValue(cmd.Env, "CODEX_HOME"); got != explicitCodexHome {
		t.Fatalf("CODEX_HOME=%q want %q", got, explicitCodexHome)
	}
	data, err := os.ReadFile(filepath.Join(explicitCodexHome, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if string(data) != authJSON {
		t.Fatalf("auth.json content = %q, want %q", string(data), authJSON)
	}
}

func TestCodexBuildCommand_WithCodexAuthUsesPreparedEnv(t *testing.T) {
	clearCodexEnv(t)
	authJSON := lifecycleSeed
	explicitCodexHome := filepath.Join(t.TempDir(), "prepared-codex-home")

	p := newTestCodexProvider()
	t.Cleanup(p.Cleanup)

	cfg := bridge.SessionConfig{
		ProjectID: "test",
		SessionID: "s1",
		RepoPath:  t.TempDir(),
		Env: []string{
			"CODEX_AUTH=" + authJSON,
			"CODEX_HOME=" + explicitCodexHome,
		},
	}
	cmd, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	if got := envValue(cmd.Env, "CODEX_HOME"); got != explicitCodexHome {
		t.Fatalf("CODEX_HOME=%q want %q", got, explicitCodexHome)
	}
	data, err := os.ReadFile(filepath.Join(explicitCodexHome, "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if string(data) != authJSON {
		t.Fatalf("auth.json content = %q, want %q", string(data), authJSON)
	}
}

func TestCodexBuildCommand_WithAPIKey(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-test")

	p := newTestCodexProvider()
	t.Cleanup(p.Cleanup)

	cfg := bridge.SessionConfig{
		ProjectID: "test",
		SessionID: "s1",
		RepoPath:  t.TempDir(),
	}
	cmd, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	if envValue(cmd.Env, "CODEX_HOME") == "" {
		t.Fatal("API-key credentials require a native auth file")
	}
}

func TestCodexCleanup(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("CODEX_AUTH", lifecycleSeed)

	p := newTestCodexProvider()

	cfg := bridge.SessionConfig{
		ProjectID: "test",
		SessionID: "s1",
		RepoPath:  t.TempDir(),
	}
	cmd, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	dir := envValue(cmd.Env, "CODEX_HOME")
	if dir == "" {
		t.Fatal("authDir not set after BuildCommand")
	}

	p.Cleanup()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("authDir should persist after Cleanup, stat err: %v", err)
	}
}
