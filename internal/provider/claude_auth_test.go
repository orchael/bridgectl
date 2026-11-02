package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestClaudeProvider pins the platform and keychain probe so provider
// results never depend on the host's real login keychain.
func newTestClaudeProvider(platform string, keychain func(string) bool) *ClaudeProvider {
	p := NewClaudeProvider(StdioConfig{
		ProviderID:   "claude",
		Binary:       "/bin/echo",
		StartupProbe: "none",
		// A configured required_env must not survive into the provider:
		// ClaudeProvider owns auth validation.
		RequiredEnv: []string{"CLAUDE_CODE_OAUTH_TOKEN"},
	})
	p.resolveAuth = func(env []string) (claudeAuthSource, error) {
		return resolveClaudeAuthWith(env, platform, keychain)
	}
	return p
}

func clearClaudeEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	_ = os.Unsetenv("CLAUDE_CODE_OAUTH_TOKEN")
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// writeClaudeCredentials seeds $HOME/.claude/.credentials.json with body.
func writeClaudeCredentials(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, claudeCredentialsFile)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

func noKeychain(string) bool { return false }

func hasKeychain(string) bool { return true }

func TestResolveClaudeAuth_TokenWins(t *testing.T) {
	env := []string{"CLAUDE_CODE_OAUTH_TOKEN=tok", "HOME=/nonexistent"}
	got, err := resolveClaudeAuthWith(env, "linux", noKeychain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.kind != claudeAuthToken {
		t.Fatalf("kind = %q, want %q", got.kind, claudeAuthToken)
	}
}

func TestResolveClaudeAuth_CredentialsFile(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"nested envelope", `{"claudeAiOauth":{"accessToken":"abc"}}`, true},
		{"bare access token", `{"accessToken":"abc"}`, true},
		{"empty nested token", `{"claudeAiOauth":{"accessToken":"   "}}`, false},
		{"logged out placeholder", `{}`, false},
		{"malformed json", `{not json`, false},
		{"empty file", ``, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := writeClaudeCredentials(t, home, tc.body)
			env := []string{"HOME=" + home}

			got, err := resolveClaudeAuthWith(env, "linux", noKeychain)
			if !tc.want {
				if err == nil {
					t.Fatalf("expected unavailable for %q, got kind %q", tc.body, got.kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.kind != claudeAuthCredentials {
				t.Fatalf("kind = %q, want %q", got.kind, claudeAuthCredentials)
			}
			if got.path != path {
				t.Fatalf("path = %q, want %q", got.path, path)
			}
		})
	}
}

// macOS keeps the credential in the login keychain, so a file-only check would
// report a signed-in developer as unauthenticated.
func TestResolveClaudeAuth_KeychainOnDarwin(t *testing.T) {
	env := []string{"HOME=" + t.TempDir()}

	got, err := resolveClaudeAuthWith(env, "darwin", hasKeychain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.kind != claudeAuthKeychain {
		t.Fatalf("kind = %q, want %q", got.kind, claudeAuthKeychain)
	}
}

func TestResolveClaudeAuth_KeychainIgnoredOffDarwin(t *testing.T) {
	env := []string{"HOME=" + t.TempDir()}

	if _, err := resolveClaudeAuthWith(env, "linux", hasKeychain); err == nil {
		t.Fatal("keychain must not satisfy auth on non-darwin hosts")
	}
}

func TestResolveClaudeAuth_NoCredentials(t *testing.T) {
	env := []string{"HOME=" + t.TempDir()}

	_, err := resolveClaudeAuthWith(env, "darwin", noKeychain)
	if err == nil {
		t.Fatal("expected error when no credential source resolves")
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("error should name the token var, got: %v", err)
	}
	if !strings.Contains(err.Error(), "setup-token") {
		t.Fatalf("error should suggest a remedy, got: %v", err)
	}
}

func TestResolveClaudeAuth_WindowsUserProfile(t *testing.T) {
	home := t.TempDir()
	writeClaudeCredentials(t, home, `{"claudeAiOauth":{"accessToken":"abc"}}`)
	env := []string{"USERPROFILE=" + home}

	got, err := resolveClaudeAuthWith(env, "windows", noKeychain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.kind != claudeAuthCredentials {
		t.Fatalf("kind = %q, want %q", got.kind, claudeAuthCredentials)
	}
}

func TestResolveClaudeAuth_NoHome(t *testing.T) {
	if _, err := resolveClaudeAuthWith(nil, "linux", noKeychain); err == nil {
		t.Fatal("expected error when HOME is unset and no token is present")
	}
}

// A configured required_env must not reject a host authenticated through the
// CLI's own store — the regression this provider exists to fix.
func TestClaudeProviderHealth_CredentialsFileSatisfiesConfiguredRequiredEnv(t *testing.T) {
	home := clearClaudeEnv(t)
	writeClaudeCredentials(t, home, `{"claudeAiOauth":{"accessToken":"abc"}}`)
	p := newTestClaudeProvider("darwin", noKeychain)

	if err := p.HealthWithEnv(context.Background(), []string{"HOME=" + home}); err != nil {
		t.Fatalf("expected healthy with native credentials, got: %v", err)
	}
}

func TestClaudeProviderHealth_TokenOnly(t *testing.T) {
	clearClaudeEnv(t)
	p := newTestClaudeProvider("linux", noKeychain)

	env := []string{"CLAUDE_CODE_OAUTH_TOKEN=tok", "HOME=" + t.TempDir()}
	if err := p.HealthWithEnv(context.Background(), env); err != nil {
		t.Fatalf("expected healthy with token, got: %v", err)
	}
}

func TestClaudeProviderHealth_NoAuth(t *testing.T) {
	clearClaudeEnv(t)
	p := newTestClaudeProvider("darwin", noKeychain)

	err := p.HealthWithEnv(context.Background(), []string{"HOME=" + t.TempDir()})
	if err == nil {
		t.Fatal("expected unavailable when no credential source resolves")
	}
	if !strings.Contains(err.Error(), "claude requires") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestClaudeProviderValidateStartup_NoAuth(t *testing.T) {
	clearClaudeEnv(t)
	p := newTestClaudeProvider("darwin", noKeychain)

	if err := p.ValidateStartup(context.Background()); err == nil {
		t.Fatal("expected ValidateStartup to fail with no credentials")
	}
}

// Health must not consult the real keychain on a host that has no credential
// file: the default resolver is what production uses, so pin the probe and
// assert the provider threads it through rather than ignoring it.
func TestClaudeProviderHealth_KeychainSatisfiesAuth(t *testing.T) {
	clearClaudeEnv(t)
	p := newTestClaudeProvider("darwin", hasKeychain)

	if err := p.HealthWithEnv(context.Background(), []string{"HOME=" + t.TempDir()}); err != nil {
		t.Fatalf("expected healthy via keychain, got: %v", err)
	}
}

func TestNewClaudeProviderClearsRequiredEnv(t *testing.T) {
	p := newTestClaudeProvider("linux", noKeychain)

	if len(p.cfg.RequiredEnv) != 0 {
		t.Fatalf("RequiredEnv = %v, want cleared", p.cfg.RequiredEnv)
	}
}

// The default resolver is what production uses. A token in the process
// environment short-circuits before any file or keychain access, so this
// covers resolveClaudeAuth/Health without depending on the host.
func TestResolveClaudeAuth_DefaultResolverTokenBranch(t *testing.T) {
	got, err := resolveClaudeAuth([]string{"CLAUDE_CODE_OAUTH_TOKEN=tok"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.kind != claudeAuthToken {
		t.Fatalf("kind = %q, want %q", got.kind, claudeAuthToken)
	}
}

func TestClaudeProviderHealth_UsesProcessEnv(t *testing.T) {
	clearClaudeEnv(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok")
	p := NewClaudeProvider(StdioConfig{
		ProviderID:   "claude",
		Binary:       "/bin/echo",
		StartupProbe: "none",
	})

	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("expected healthy with token in process env, got: %v", err)
	}
}

// An absent service must read as "no credential", never as an error or a
// prompt. Off darwin the `security` lookup is simply unavailable, which must
// reach the same answer.
func TestClaudeKeychainLookup_MissingService(t *testing.T) {
	if claudeKeychainLookup("bridgectl-test-service-that-does-not-exist") {
		t.Fatal("expected no keychain entry for a nonexistent service")
	}
}

func TestNewClaudeChatProviderClearsRequiredEnv(t *testing.T) {
	p := NewClaudeChatProvider()

	if len(p.cfg.RequiredEnv) != 0 {
		t.Fatalf("RequiredEnv = %v, want cleared", p.cfg.RequiredEnv)
	}
}
