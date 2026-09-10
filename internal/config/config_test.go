package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	content := `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  echo:
    binary: "cat"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Input.MaxSizeBytes == 0 {
		t.Fatal("expected default input.max_size_bytes")
	}
	if cfg.RateLimits.GlobalRPS == 0 || cfg.RateLimits.GlobalBurst == 0 {
		t.Fatal("expected default global rate limits")
	}
}

func TestLoadValidateBadDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	content := `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "bad"
providers:
  echo:
    binary: "cat"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "jwt_max_ttl") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadValidateBadRequiredEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	content := `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  claude:
    binary: "claude"
    required_env: [""]
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "required_env") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadValidateProviderFallbacks(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "accepts known fallbacks",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  primary:
    binary: "cat"
    fallbacks: ["secondary", "tertiary"]
  secondary:
    binary: "cat"
  tertiary:
    binary: "cat"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
		},
		{
			name: "rejects more than two fallbacks",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  primary:
    binary: "cat"
    fallbacks: ["a", "b", "c"]
  a:
    binary: "cat"
  b:
    binary: "cat"
  c:
    binary: "cat"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantErr: "must have at most 2 entries",
		},
		{
			name: "rejects self fallback",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  primary:
    binary: "cat"
    fallbacks: ["primary"]
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantErr: "provider cannot be its own fallback",
		},
		{
			name: "rejects unknown fallback",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  primary:
    binary: "cat"
    fallbacks: ["missing"]
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantErr: `unknown provider "missing"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			cfg, err := Load(path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if got := cfg.Providers["primary"].Fallbacks; len(got) != 2 || got[0] != "secondary" || got[1] != "tertiary" {
					t.Fatalf("Fallbacks=%v want [secondary tertiary]", got)
				}
				return
			}

			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadFeatureFlags(t *testing.T) {
	t.Run("provider_fallbacks true", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bridge.yaml")
		content := `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
feature_flags:
  provider_fallbacks: true
providers:
  primary:
    binary: "cat"
    fallbacks: ["secondary"]
  secondary:
    binary: "cat"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.FeatureFlags.ProviderFallbacks {
			t.Fatal("expected provider_fallbacks to be true")
		}
	})

	t.Run("provider_fallbacks defaults to false", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bridge.yaml")
		content := `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  primary:
    binary: "cat"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.FeatureFlags.ProviderFallbacks {
			t.Fatal("expected provider_fallbacks to default to false")
		}
	})
}

func TestLoadRepoSetupDefaultsAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "defaults",
			content: `
server:
  listen: "127.0.0.1:9445"
providers:
  echo:
    binary: "cat"
`,
		},
		{
			name: "explicit valid config",
			content: `
server:
  listen: "127.0.0.1:9445"
repo_setup:
  enabled: false
  config_path: ".bridgectl.yaml"
  default_timeout: "1m"
  max_timeout: "10m"
providers:
  echo:
    binary: "cat"
`,
		},
		{
			name: "absolute path rejected",
			content: `
server:
  listen: "127.0.0.1:9445"
repo_setup:
  config_path: "/tmp/setup.yaml"
providers:
  echo:
    binary: "cat"
`,
			wantErr: "config_path must be relative",
		},
		{
			name: "parent component rejected",
			content: `
server:
  listen: "127.0.0.1:9445"
repo_setup:
  config_path: "../setup.yaml"
providers:
  echo:
    binary: "cat"
`,
			wantErr: "must not contain '..'",
		},
		{
			name: "default exceeds max",
			content: `
server:
  listen: "127.0.0.1:9445"
repo_setup:
  default_timeout: "20m"
  max_timeout: "10m"
providers:
  echo:
    binary: "cat"
`,
			wantErr: "default_timeout must not exceed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			cfg, err := Load(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load error=%v want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RepoSetup.ConfigPath != ".bridgectl.yaml" {
				t.Fatalf("ConfigPath=%q want .bridgectl.yaml", cfg.RepoSetup.ConfigPath)
			}
			if cfg.RepoSetup.DefaultTimeout == "" || cfg.RepoSetup.MaxTimeout == "" {
				t.Fatalf("repo setup timeouts not defaulted: %+v", cfg.RepoSetup)
			}
		})
	}
}

func TestRepoSetupIsEnabled(t *testing.T) {
	if !(RepoSetupConfig{}).IsEnabled() {
		t.Fatal("nil enabled should default true")
	}
	disabled := false
	if (RepoSetupConfig{Enabled: &disabled}).IsEnabled() {
		t.Fatal("explicit false should disable repo setup")
	}
}

func TestHasExplicitServerListen(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "missing", body: "providers: {}\n", want: false},
		{name: "empty", body: "server:\n  listen: \"\"\n", want: false},
		{name: "set", body: "server:\n  listen: \"127.0.0.1:9445\"\n", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			got, err := HasExplicitServerListen(path)
			if err != nil {
				t.Fatalf("HasExplicitServerListen: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasExplicitServerListen=%v want %v", got, tc.want)
			}
		})
	}
}

func TestLoadStepCAClients(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	content := `
server:
  listen: "127.0.0.1:9445"
step_ca:
  url: "https://step-ca.example.internal"
  root: "/etc/bridge/step-root.crt"
  clients:
    - issuer: "do-dev2"
      key_path: "/etc/bridge/jwt-clients/do-dev2.pub"
    - issuer: "laptop-a"
      required: true
providers:
  echo:
    binary: "cat"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cfg.StepCA.Clients); got != 2 {
		t.Fatalf("len(StepCA.Clients)=%d want 2", got)
	}
	if cfg.StepCA.Clients[0].Issuer != "do-dev2" {
		t.Fatalf("first issuer=%q want do-dev2", cfg.StepCA.Clients[0].Issuer)
	}
	if cfg.StepCA.Clients[0].KeyPath != "/etc/bridge/jwt-clients/do-dev2.pub" {
		t.Fatalf("first key_path=%q", cfg.StepCA.Clients[0].KeyPath)
	}
	if !cfg.StepCA.Clients[1].Required {
		t.Fatal("second client should be required")
	}
}

func TestLoadStepCAClientsValidateIssuer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	content := `
server:
  listen: "127.0.0.1:9445"
step_ca:
  clients:
    - issuer: "../bad"
providers:
  echo:
    binary: "cat"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "step_ca.clients[0].issuer") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsDeprecatedPTYField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	content := `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  primary:
    binary: "cat"
    pty: true
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), ".pty is no longer supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoadRejectsDeprecatedModeField verifies that setting mode on a provider
// (e.g. mode: exec) produces a clear startup error. This was the original
// dispatch bug described in issue #4: mode: exec silently wired any provider
// to the Codex-specific exec implementation.
func TestLoadRejectsDeprecatedModeField(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "mode exec rejected",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  claude:
    binary: "claude"
    mode: exec
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantErr: ".mode is no longer supported",
		},
		{
			name: "mode stdio rejected",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  codex:
    binary: "codex"
    mode: stdio
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantErr: ".mode is no longer supported",
		},
		{
			name: "stream_json accepted without mode",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
providers:
  codex:
    binary: "codex"
    stream_json: true
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			cfg, err := Load(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !cfg.Providers["codex"].StreamJSON {
				t.Fatal("expected stream_json to be true")
			}
		})
	}
}

func TestLoadRuntimeProviderRoot(t *testing.T) {
	homeDir := t.TempDir()
	xdgDir := filepath.Join(t.TempDir(), "xdg-data")
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", xdgDir)

	tests := []struct {
		name     string
		content  string
		wantRoot string
		wantErr  bool
	}{
		{
			name: "provider_root set",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
runtime:
  provider_root: "/opt/bridgectl"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantRoot: "/opt/bridgectl",
		},
		{
			name: "provider_root expands braced home variable",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
runtime:
  provider_root: "${HOME}/.local/share/bridgectl/providers"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantRoot: filepath.Join(homeDir, ".local/share/bridgectl/providers"),
		},
		{
			name: "provider_root expands home variable",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
runtime:
  provider_root: "$HOME/.local/share/bridgectl/providers"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantRoot: filepath.Join(homeDir, ".local/share/bridgectl/providers"),
		},
		{
			name: "provider_root expands xdg data variable",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
runtime:
  provider_root: "$XDG_DATA_HOME/bridgectl/providers"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantRoot: filepath.Join(xdgDir, "bridgectl/providers"),
		},
		{
			name: "provider_root expands braced xdg data variable",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
runtime:
  provider_root: "${XDG_DATA_HOME}/bridgectl/providers"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantRoot: filepath.Join(xdgDir, "bridgectl/providers"),
		},
		{
			name: "provider_root absent",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantRoot: "",
		},
		{
			name: "provider_root relative path rejected",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
runtime:
  provider_root: "relative/path"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantErr: true,
		},
		{
			name: "provider_root unresolved variable rejected",
			content: `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
runtime:
  provider_root: "$BRIDGE_PROVIDER_ROOT/providers"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			cfg, err := Load(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Runtime.ProviderRoot != tc.wantRoot {
				t.Fatalf("ProviderRoot=%q want %q", cfg.Runtime.ProviderRoot, tc.wantRoot)
			}
		})
	}
}

func TestLoadValidateCertDurations(t *testing.T) {
	base := `
server:
  listen: "127.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
providers:
  echo:
    binary: "cat"
`

	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{
			name:  "valid cert_validity",
			extra: "  cert_validity: \"30m\"\n",
		},
		{
			name:  "valid cert_renewal_check_interval",
			extra: "  cert_renewal_check_interval: \"10m\"\n",
		},
		{
			name:    "invalid cert_validity",
			extra:   "  cert_validity: \"not-a-duration\"\n",
			wantErr: "server.cert_validity",
		},
		{
			name:    "invalid cert_renewal_check_interval",
			extra:   "  cert_renewal_check_interval: \"bad\"\n",
			wantErr: "server.cert_renewal_check_interval",
		},
		{
			name:    "negative cert_validity",
			extra:   "  cert_validity: \"-5m\"\n",
			wantErr: "must not be negative",
		},
		{
			name:    "negative cert_renewal_check_interval",
			extra:   "  cert_renewal_check_interval: \"-1h\"\n",
			wantErr: "must not be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Insert extra fields under the server: block.
			content := strings.Replace(base, "  listen:", tc.extra+"  listen:", 1)
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := Load(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

// --- SecurityConfig synthesis and validation tests ---

// minimalYAML is the smallest valid config with the given extra content
// prepended to the YAML.
func writeTestConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	content := extra + `
providers:
  echo:
    binary: "cat"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestSecuritySynthesis_DefaultsToLocal(t *testing.T) {
	// No server.listen, no step_ca, no tls — should synthesize local mode.
	// Note: applyDefaults sets server.listen to "0.0.0.0:9445", but since
	// synthesizeSecurity runs after applyDefaults and checks for the
	// default value, it still treats the default listen as "no explicit listen".
	path := writeTestConfig(t, `
server:
  listen: "0.0.0.0:9445"
auth:
  jwt_max_ttl: "5m"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Transport.Mode != TransportModeLocal {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Security.Transport.Mode, TransportModeLocal)
	}
	if cfg.Security.Authorization.Mode != "none" {
		t.Errorf("Authorization.Mode = %q, want %q", cfg.Security.Authorization.Mode, "none")
	}
	if cfg.Security.Certificates.Provider != "auto" {
		t.Errorf("Certificates.Provider = %q, want %q", cfg.Security.Certificates.Provider, "auto")
	}
}

func TestSecuritySynthesis_StepCA(t *testing.T) {
	path := writeTestConfig(t, `
server:
  listen: "10.0.0.1:9445"
step_ca:
  url: "https://step-ca.internal:443"
  root: "/etc/step/root.crt"
  provisioner: "bridge-jwk"
auth:
  jwt_max_ttl: "5m"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Transport.Mode != TransportModeMTLS {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Security.Transport.Mode, TransportModeMTLS)
	}
	if cfg.Security.Certificates.Provider != "stepca" {
		t.Errorf("Certificates.Provider = %q, want %q", cfg.Security.Certificates.Provider, "stepca")
	}
	if cfg.Security.Certificates.StepCA.URL != "https://step-ca.internal:443" {
		t.Errorf("StepCA.URL = %q, want %q", cfg.Security.Certificates.StepCA.URL, "https://step-ca.internal:443")
	}
	if cfg.Security.Authorization.Mode != "jwt" {
		t.Errorf("Authorization.Mode = %q, want %q", cfg.Security.Authorization.Mode, "jwt")
	}
}

func TestSecuritySynthesis_ExplicitTLS(t *testing.T) {
	path := writeTestConfig(t, `
server:
  listen: "10.0.0.1:9445"
tls:
  ca_bundle: "/etc/bridgectl/ca.pem"
  cert: "/etc/bridgectl/server.crt"
  key: "/etc/bridgectl/server.key"
auth:
  jwt_max_ttl: "5m"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Transport.Mode != TransportModeMTLS {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Security.Transport.Mode, TransportModeMTLS)
	}
	if cfg.Security.Certificates.Provider != "filesystem" {
		t.Errorf("Certificates.Provider = %q, want %q", cfg.Security.Certificates.Provider, "filesystem")
	}
	if cfg.Security.Certificates.Filesystem.CA != "/etc/bridgectl/ca.pem" {
		t.Errorf("Filesystem.CA = %q, want %q", cfg.Security.Certificates.Filesystem.CA, "/etc/bridgectl/ca.pem")
	}
}

func TestSecuritySynthesis_AutoPKI(t *testing.T) {
	// Non-default listen but no step_ca or tls → auto provider.
	path := writeTestConfig(t, `
server:
  listen: "10.0.0.1:9445"
auth:
  jwt_max_ttl: "5m"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Transport.Mode != TransportModeMTLS {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Security.Transport.Mode, TransportModeMTLS)
	}
	if cfg.Security.Certificates.Provider != "auto" {
		t.Errorf("Certificates.Provider = %q, want %q", cfg.Security.Certificates.Provider, "auto")
	}
}

func TestSecurityExplicit_NewFormat(t *testing.T) {
	// Explicit security block should be used as-is without synthesis.
	path := writeTestConfig(t, `
server:
  listen: "10.0.0.1:9445"
security:
  transport:
    mode: tls
  certificates:
    provider: filesystem
    filesystem:
      ca: "/ca.pem"
      server_certificate: "/server.crt"
      server_private_key: "/server.key"
  authorization:
    mode: jwt
auth:
  jwt_max_ttl: "5m"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.Transport.Mode != TransportModeTLS {
		t.Errorf("Transport.Mode = %q, want %q", cfg.Security.Transport.Mode, TransportModeTLS)
	}
	if cfg.Security.Certificates.Provider != "filesystem" {
		t.Errorf("Certificates.Provider = %q, want %q", cfg.Security.Certificates.Provider, "filesystem")
	}
	if cfg.Security.Certificates.Filesystem.ServerCert != "/server.crt" {
		t.Errorf("Filesystem.ServerCert = %q, want %q", cfg.Security.Certificates.Filesystem.ServerCert, "/server.crt")
	}
	if cfg.Security.Authorization.Mode != "jwt" {
		t.Errorf("Authorization.Mode = %q, want %q", cfg.Security.Authorization.Mode, "jwt")
	}
}

func TestSecurityValidation_InvalidMode(t *testing.T) {
	path := writeTestConfig(t, `
server:
  listen: "10.0.0.1:9445"
security:
  transport:
    mode: "insecure"
auth:
  jwt_max_ttl: "5m"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for invalid transport mode")
	}
	if !strings.Contains(err.Error(), "security.transport.mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSecurityValidation_InvalidProvider(t *testing.T) {
	path := writeTestConfig(t, `
server:
  listen: "10.0.0.1:9445"
security:
  transport:
    mode: mtls
  certificates:
    provider: "vault"
auth:
  jwt_max_ttl: "5m"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for unsupported provider")
	}
	if !strings.Contains(err.Error(), "security.certificates.provider") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantStart int
		wantEnd   int
		wantErr   string
	}{
		{
			name:      "valid range",
			input:     "4100-4199",
			wantStart: 4100,
			wantEnd:   4200,
		},
		{
			name:      "single port",
			input:     "8080-8080",
			wantStart: 8080,
			wantEnd:   8081,
		},
		{
			name:    "missing separator",
			input:   "4100",
			wantErr: "'start-end' format",
		},
		{
			name:    "non-numeric start",
			input:   "abc-4200",
			wantErr: "invalid port range start",
		},
		{
			name:    "non-numeric end",
			input:   "4100-xyz",
			wantErr: "invalid port range end",
		},
		{
			name:    "start greater than end",
			input:   "5000-4000",
			wantErr: "start must be <= end",
		},
		{
			name:    "zero start",
			input:   "0-100",
			wantErr: "start must be <= end and both > 0",
		},
		{
			name:    "negative start",
			input:   "-1-100",
			wantErr: "invalid port range start",
		},
		{
			name:    "exceeds max port start",
			input:   "70000-70010",
			wantErr: "must be <= 65535",
		},
		{
			name:    "exceeds max port end",
			input:   "4100-70000",
			wantErr: "must be <= 65535",
		},
		{
			name:      "max valid port",
			input:     "65534-65535",
			wantStart: 65534,
			wantEnd:   65536,
		},
		{
			name:      "with whitespace around numbers",
			input:     " 4100 - 4199 ",
			wantStart: 4100,
			wantEnd:   4200,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := ParsePortRange(tc.input)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePortRange(%q) expected error containing %q, got nil", tc.input, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParsePortRange(%q) error = %v, want %q", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePortRange(%q) unexpected error: %v", tc.input, err)
			}
			if start != tc.wantStart {
				t.Errorf("ParsePortRange(%q) start = %d, want %d", tc.input, start, tc.wantStart)
			}
			if end != tc.wantEnd {
				t.Errorf("ParsePortRange(%q) end = %d, want %d", tc.input, end, tc.wantEnd)
			}
		})
	}
}

func TestSecurityValidation_InvalidAuthMode(t *testing.T) {
	path := writeTestConfig(t, `
server:
  listen: "10.0.0.1:9445"
security:
  transport:
    mode: mtls
  authorization:
    mode: "oauth"
auth:
  jwt_max_ttl: "5m"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error for unsupported auth mode")
	}
	if !strings.Contains(err.Error(), "security.authorization.mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}
