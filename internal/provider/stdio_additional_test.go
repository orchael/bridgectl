package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewStdioProviderDefaultsAndHealth(t *testing.T) {
	p := NewStdioProvider(StdioConfig{
		ProviderID: "echo",
		Binary:     "/bin/echo",
	})

	if got := p.StartupTimeout().Seconds(); got <= 0 {
		t.Fatalf("StartupTimeout=%v want > 0", p.StartupTimeout())
	}
	if got := p.StopGrace().Seconds(); got <= 0 {
		t.Fatalf("StopGrace=%v want > 0", p.StopGrace())
	}
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	version, err := p.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version == "" {
		t.Fatal("Version was empty")
	}
}

func TestHealthRequiredEnv(t *testing.T) {
	const key = "BRIDGE_HEALTH_REQUIRED_ENV"
	_ = os.Unsetenv(key)

	p := NewStdioProvider(StdioConfig{
		ProviderID:  "echo",
		Binary:      "/bin/echo",
		RequiredEnv: []string{key},
	})

	err := p.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), key) {
		t.Fatalf("Health with missing env: got %v, want error containing %q", err, key)
	}

	t.Setenv(key, "present")
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health with env set: %v", err)
	}

	_ = os.Unsetenv(key)
	if err := p.HealthWithEnv(context.Background(), []string{"PATH=" + os.Getenv("PATH"), key + "=from-setup"}); err != nil {
		t.Fatalf("HealthWithEnv with setup env: %v", err)
	}
}

func TestValidateStartupRequiredEnv(t *testing.T) {
	const key = "BRIDGE_PROVIDER_REQUIRED_ENV"
	_ = os.Unsetenv(key)

	p := NewStdioProvider(StdioConfig{
		ProviderID:   "fake",
		Binary:       "/bin/echo",
		StartupProbe: "none",
		RequiredEnv:  []string{key},
	})
	err := p.ValidateStartup(context.Background())
	if err == nil || !strings.Contains(err.Error(), key) {
		t.Fatalf("ValidateStartup error=%v want missing %s", err, key)
	}

	if err := os.Setenv(key, "present"); err != nil {
		t.Fatalf("Setenv: %v", err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(key) })

	if err := p.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("ValidateStartup with env: %v", err)
	}
}

func TestResolveBinaryPathAndFilterEnv(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "tool")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path, err := resolveBinaryPath(bin, "")
	if err != nil {
		t.Fatalf("resolveBinaryPath: %v", err)
	}
	if path != bin {
		t.Fatalf("resolveBinaryPath=%q want %q", path, bin)
	}

	env := filterEnv([]string{
		"AWS_SECRET_ACCESS_KEY=secret",
		"KEEP=value",
	})
	for _, item := range env {
		if strings.HasPrefix(item, "AWS_SECRET_ACCESS_KEY=") {
			t.Fatalf("blocked secret env leaked: %v", env)
		}
	}
	if !hasEnvKey(env, "TERM") || !hasEnvKey(env, "COLORTERM") {
		t.Fatalf("TERM and COLORTERM were not injected: %v", env)
	}
}

func TestVersionProbeEnvExcludesAuthTokens(t *testing.T) {
	// Auth tokens present in the process environment must not appear in the
	// version probe environment, so that provider binaries which validate
	// credentials on startup (triggering network round-trips) do not time out
	// during the version check.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("OPENAI_API_KEY", "sk-open-secret")

	env := versionProbeEnv()

	// Assert by key name — more robust than value substring matching, and
	// catches the case where the key is present with a different value.
	for _, key := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if hasEnvKey(env, key) {
			t.Errorf("auth token key %q must not appear in version probe env", key)
		}
	}
	if !hasEnvKey(env, "PATH") {
		t.Error("PATH missing from version probe env")
	}
	if !hasEnvKey(env, "TERM") {
		t.Error("TERM missing from version probe env")
	}
}

func TestVersionUsesMinimalEnv(t *testing.T) {
	// Build a script that prints its environment to stdout so we can verify
	// that auth tokens are not passed to --version subprocesses.
	tmp := t.TempDir()
	script := filepath.Join(tmp, "fakebin")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-must-not-leak")

	p := NewStdioProvider(StdioConfig{
		ProviderID: "fake",
		Binary:     script,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := p.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if strings.Contains(out, "tok-must-not-leak") {
		t.Errorf("auth token leaked into version probe output: %q", out)
	}
}

func TestValidateStartupOutputAndPrompt(t *testing.T) {
	script := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'READY>\\n'\nsleep 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	outputProvider := NewStdioProvider(StdioConfig{
		ProviderID:     "output",
		Binary:         script,
		StartupProbe:   "output",
		StartupTimeout: 2 * time.Second,
	})
	if err := outputProvider.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("ValidateStartup output: %v", err)
	}

	promptProvider := NewStdioProvider(StdioConfig{
		ProviderID:     "prompt",
		Binary:         script,
		StartupProbe:   "prompt",
		PromptPattern:  "READY>",
		StartupTimeout: 2 * time.Second,
	})
	if err := promptProvider.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("ValidateStartup prompt: %v", err)
	}

	badProvider := NewStdioProvider(StdioConfig{
		ProviderID:   "bad",
		Binary:       "/bin/echo",
		StartupProbe: "unsupported",
	})
	if err := badProvider.ValidateStartup(context.Background()); err == nil {
		t.Fatal("ValidateStartup accepted unsupported probe")
	}
}

func TestStdioProvider_Accessors(t *testing.T) {
	p := NewStdioProvider(StdioConfig{
		ProviderID: "my-provider",
		Binary:     "/usr/bin/env",
		StreamJSON: true,
		StripANSI:  true,
	})

	if got := p.ID(); got != "my-provider" {
		t.Errorf("ID() = %q, want %q", got, "my-provider")
	}
	if got := p.Binary(); got != "/usr/bin/env" {
		t.Errorf("Binary() = %q, want %q", got, "/usr/bin/env")
	}
	if !p.IsStreamJSON() {
		t.Error("IsStreamJSON() = false, want true")
	}
	if !p.IsStripANSI() {
		t.Error("IsStripANSI() = false, want true")
	}
}

func TestStdioProvider_IsStreamJSON_False(t *testing.T) {
	p := NewStdioProvider(StdioConfig{
		ProviderID: "plain",
		Binary:     "/bin/echo",
		StreamJSON: false,
	})
	if p.IsStreamJSON() {
		t.Error("IsStreamJSON() = true, want false for StreamJSON:false config")
	}
}

func TestStdioProvider_IsStripANSI_False(t *testing.T) {
	p := NewStdioProvider(StdioConfig{
		ProviderID: "plain",
		Binary:     "/bin/echo",
		StripANSI:  false,
	})
	if p.IsStripANSI() {
		t.Error("IsStripANSI() = true, want false for StripANSI:false config")
	}
}

func TestStdioProvider_SetUnavailable(t *testing.T) {
	p := NewStdioProvider(StdioConfig{
		ProviderID: "probe",
		Binary:     "/bin/echo",
	})

	// Health should pass before SetUnavailable.
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health before SetUnavailable: %v", err)
	}

	// Mark the provider as unavailable.
	p.SetUnavailable(fmt.Errorf("startup probe failed"))

	// Health should now return an error.
	if err := p.Health(context.Background()); err == nil {
		t.Error("Health after SetUnavailable returned nil, want error")
	}
}

func TestAbsRoot_RelativePath(t *testing.T) {
	// absRoot on a relative path should return an absolute path.
	got, err := absRoot("relative/path")
	if err != nil {
		t.Fatalf("absRoot: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("absRoot returned non-absolute path: %q", got)
	}
}

func TestAbsRoot_AbsolutePath(t *testing.T) {
	got, err := absRoot("/absolute/path")
	if err != nil {
		t.Fatalf("absRoot: %v", err)
	}
	if got != "/absolute/path" {
		t.Errorf("absRoot = %q, want %q", got, "/absolute/path")
	}
}

func TestIsStandaloneRelativePathArg(t *testing.T) {
	cases := []struct {
		arg  string
		want bool
	}{
		{".", true},
		{"..", true},
		{"./foo", true},
		{"../bar", true},
		{"--flag", false},
		{"--config=./foo", false},
		{"plain-arg", false},
		{"/absolute", false},
	}
	for _, tc := range cases {
		got := isStandaloneRelativePathArg(tc.arg)
		if got != tc.want {
			t.Errorf("isStandaloneRelativePathArg(%q) = %v, want %v", tc.arg, got, tc.want)
		}
	}
}
