package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

const lifecycleSeed = `{"auth_mode":"chatgpt","tokens":{"access_token":"seed","refresh_token":"refresh","id_token":"id"}}`
const lifecycleRefreshed = `{"auth_mode":"chatgpt","tokens":{"access_token":"new","refresh_token":"new-refresh","id_token":"id"}}`

func writeLifecycleAuth(t *testing.T, home, contents string) {
	t.Helper()
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

// CA-1/2: Existing accounts and provider-refreshed state outrank the bootstrap.
func TestCodexLifecyclePreservesAccountAcrossRestart(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("CODEX_AUTH", lifecycleSeed)
	first := newTestCodexProvider()
	cfg := bridge.SessionConfig{RepoPath: t.TempDir()}
	cmd, err := first.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	home := envValue(cmd.Env, "CODEX_HOME")
	writeLifecycleAuth(t, home, lifecycleRefreshed)
	for _, p := range []*CodexProvider{first, newTestCodexProvider()} {
		if _, err := p.BuildCommand(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(home, "auth.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != lifecycleRefreshed {
			t.Fatal("refreshed account was overwritten by bootstrap")
		}
	}
}

func TestCodexLifecycleNativeAccountWins(t *testing.T) {
	clearCodexEnv(t)
	home := filepath.Join(os.Getenv("HOME"), ".codex")
	writeLifecycleAuth(t, home, lifecycleRefreshed)
	t.Setenv("CODEX_AUTH", lifecycleSeed)
	t.Setenv("CODEX_API_KEY", "sk-key")
	t.Setenv("OPENAI_API_KEY", "sk-key")
	cmd, err := newTestCodexProvider().BuildCommand(context.Background(), bridge.SessionConfig{RepoPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if envValue(cmd.Env, "CODEX_HOME") != home {
		t.Fatal("native account did not win")
	}
	if envValue(cmd.Env, "CODEX_API_KEY") != "" || envValue(cmd.Env, "OPENAI_API_KEY") != "" {
		t.Fatal("API-key override remains in account environment")
	}
}

// CA-1: a provider cannot pin all sessions to the first CODEX_HOME.
func TestCodexLifecycleSessionHomesIsolated(t *testing.T) {
	clearCodexEnv(t)
	p := newTestCodexProvider()
	for _, seed := range []string{lifecycleSeed, lifecycleRefreshed} {
		home := t.TempDir()
		cmd, err := p.BuildCommand(context.Background(), bridge.SessionConfig{RepoPath: t.TempDir(), Env: []string{"CODEX_HOME=" + home, "CODEX_AUTH=" + seed}})
		if err != nil {
			t.Fatal(err)
		}
		if envValue(cmd.Env, "CODEX_HOME") != home {
			t.Fatal("session inherited another session's CODEX_HOME")
		}
		data, err := os.ReadFile(filepath.Join(home, "auth.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != seed {
			t.Fatal("wrong account in isolated home")
		}
	}
}

// CA-3/4: health validates the same prepared environment as session creation.
func TestCodexLifecycleHealthPreparedEnvironment(t *testing.T) {
	clearCodexEnv(t)
	p := newTestCodexProvider()
	for _, malformed := range []string{"", "secret-not-json", `{}`, `{"auth_mode":"chatgpt"}`, `{"tokens":{"access_token":"only"}}`} {
		env := []string{"HOME=" + t.TempDir(), "CODEX_AUTH=" + malformed}
		if err := p.HealthWithEnv(context.Background(), env); err == nil {
			t.Fatal("health accepted missing or malformed credentials")
		}
		if _, err := p.BuildCommand(context.Background(), bridge.SessionConfig{RepoPath: t.TempDir(), Env: env}); err == nil {
			t.Fatal("command accepted missing or malformed credentials")
		}
	}
	t.Setenv("CODEX_AUTH", "invalid-daemon-seed")
	if err := p.HealthWithEnv(context.Background(), []string{"HOME=" + t.TempDir(), "CODEX_AUTH=" + lifecycleSeed}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	writeLifecycleAuth(t, home, lifecycleRefreshed)
	if err := p.HealthWithEnv(context.Background(), []string{"CODEX_HOME=" + home}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexLifecycleAPIKeyFallback(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("CODEX_AUTH", "invalid-secret")
	t.Setenv("OPENAI_API_KEY", "sk-fallback")
	cmd, err := newTestCodexProvider().BuildCommand(context.Background(), bridge.SessionConfig{RepoPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(envValue(cmd.Env, "CODEX_HOME"), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"OPENAI_API_KEY":"sk-fallback"}` {
		t.Fatal("API key was not materialized in native credential file")
	}
}

func TestCodexLifecycleStartupProbeUsesSelectedAuth(t *testing.T) {
	clearCodexEnv(t)
	t.Setenv("CODEX_AUTH", lifecycleSeed)
	t.Setenv("OPENAI_API_KEY", "sk-unused")
	p := NewCodexProvider(StdioConfig{
		ProviderID: "codex", Binary: "/bin/sh", StartupProbe: "output", StartupTimeout: time.Second,
		DefaultArgs: []string{"-c", `test -f "$CODEX_HOME/auth.json" && test -z "$OPENAI_API_KEY" && printf ready`},
	})
	if err := p.ValidateStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexLifecycleCredentialSourceSelection(t *testing.T) {
	for _, tc := range []struct {
		name, existing, seed, key, want string
		preserve                        bool
	}{
		{"empty file seeds account", "", lifecycleSeed, "", lifecycleSeed, false},
		{"invalid file seeds account", "invalid", lifecycleSeed, "", lifecycleSeed, false},
		{"account wins invalid seed", lifecycleRefreshed, "invalid", "sk-key", lifecycleRefreshed, true},
		{"seed wins cached API key", `{"OPENAI_API_KEY":"sk-old"}`, lifecycleSeed, "sk-key", lifecycleSeed, false},
		{"key replaces cached API key", `{"OPENAI_API_KEY":"sk-old"}`, "", "sk-new", `{"OPENAI_API_KEY":"sk-new"}`, false},
		{"cached API key works without env", `{"OPENAI_API_KEY":"sk-old"}`, "", "", `{"OPENAI_API_KEY":"sk-old"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearCodexEnv(t)
			home := t.TempDir()
			writeLifecycleAuth(t, home, tc.existing)
			// An explicit home must not pick a valid account from process HOME.
			writeLifecycleAuth(t, filepath.Join(os.Getenv("HOME"), ".codex"), lifecycleRefreshed)
			source, err := resolveCodexAuth([]string{"CODEX_HOME=" + home, "CODEX_AUTH=" + tc.seed, "OPENAI_API_KEY=" + tc.key})
			if err != nil {
				t.Fatal(err)
			}
			if source.home != home {
				t.Fatal("wrong credential home")
			}
			if tc.preserve {
				if len(source.seed) != 0 {
					t.Fatal("existing credential selected for overwrite")
				}
				return
			}
			if string(source.seed) != tc.want {
				t.Fatal("wrong bootstrap credential source")
			}
		})
	}
}

func TestCodexLifecycleExplicitHomeDoesNotUseOtherAccount(t *testing.T) {
	clearCodexEnv(t)
	writeLifecycleAuth(t, filepath.Join(os.Getenv("HOME"), ".codex"), lifecycleRefreshed)
	if _, err := resolveCodexAuth([]string{"HOME=" + os.Getenv("HOME"), "CODEX_HOME=" + t.TempDir()}); err == nil {
		t.Fatal("explicit home borrowed another account")
	}
	if _, err := resolveCodexAuth([]string{"CODEX_AUTH=secret-invalid-value", "CODEX_HOME=" + t.TempDir()}); err == nil || strings.Contains(err.Error(), "secret-invalid-value") {
		t.Fatal("missing error or exposed credential")
	}
}

func TestCodexLifecycleHealthRejectsNonDirectoryHome(t *testing.T) {
	clearCodexEnv(t)
	parent := t.TempDir()
	home := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(home, []byte("ordinary file"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"CODEX_HOME=" + filepath.Join(home, "nested"), "CODEX_AUTH=" + lifecycleSeed}
	if err := newTestCodexProvider().HealthWithEnv(context.Background(), env); err == nil {
		t.Fatal("health accepted a non-directory credential path")
	}
}

func TestCodexLifecycleUnwritableBootstrapParentFailsAtPreparation(t *testing.T) {
	clearCodexEnv(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	env := []string{"CODEX_HOME=" + filepath.Join(parent, "nested"), "CODEX_AUTH=" + lifecycleSeed}
	p := newTestCodexProvider()
	// Health validates credential availability without creating files. The
	// preparation step is authoritative for actual writes and their errors.
	if err := p.HealthWithEnv(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if _, err := p.BuildCommand(context.Background(), bridge.SessionConfig{RepoPath: t.TempDir(), Env: env}); err == nil {
		if os.Geteuid() == 0 {
			t.Skip("root can write despite mode restrictions")
		}
		t.Fatal("command preparation accepted an unwritable bootstrap parent")
	}
}
