package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/orchael/bridgectl/internal/bridge"
)

// CodexProvider wraps StdioProvider with Codex-specific auth handling.
// It accepts either OPENAI_API_KEY / CODEX_API_KEY (API-key auth) or
// CODEX_AUTH / CODEX_HOME auth.json (ChatGPT account auth) as valid
// authentication.
//
// CODEX_AUTH bootstraps a desktop-local auth.json once. Codex owns subsequent
// refreshes; only an explicit operator rotation should remove that file.
type CodexProvider struct {
	*StdioProvider
}

// Serialize selection and bootstrap across provider instances in this daemon.
var codexAuthMu sync.Mutex

// NewCodexProvider creates a Codex provider that supports both API-key
// and device-code authentication. RequiredEnv is cleared from the
// underlying StdioConfig because CodexProvider validates auth itself.
func NewCodexProvider(cfg StdioConfig) *CodexProvider {
	cfg.RequiredEnv = nil // handled by CodexProvider
	return &CodexProvider{
		StdioProvider: NewStdioProvider(cfg),
	}
}

func (p *CodexProvider) ValidateStartup(ctx context.Context) error {
	cmd, err := p.BuildCommand(ctx, bridge.SessionConfig{})
	if err != nil {
		return err
	}
	return p.validateStartupWithEnv(ctx, cmd.Env)
}

func (p *CodexProvider) Health(ctx context.Context) error {
	return p.HealthWithEnv(ctx, os.Environ())
}

func (p *CodexProvider) HealthWithEnv(ctx context.Context, env []string) error {
	if _, err := resolveCodexAuth(env); err != nil {
		return err
	}
	return p.StdioProvider.HealthWithEnv(ctx, env)
}

func (p *CodexProvider) BuildCommand(ctx context.Context, cfg bridge.SessionConfig) (*exec.Cmd, error) {
	cmd, err := p.StdioProvider.BuildCommand(ctx, cfg)
	if err != nil {
		return nil, err
	}

	codexAuthMu.Lock()
	defer codexAuthMu.Unlock()
	auth, err := resolveCodexAuth(cmd.Env)
	if err != nil {
		return nil, err
	}
	if len(auth.seed) > 0 {
		if err := os.MkdirAll(auth.home, 0o700); err != nil {
			return nil, fmt.Errorf("create codex auth directory: %w", err)
		}
		if err := os.Chmod(auth.home, 0o700); err != nil {
			return nil, fmt.Errorf("secure codex auth directory: %w", err)
		}
		if err := atomicWriteFile(filepath.Join(auth.home, "auth.json"), auth.seed, 0o600); err != nil {
			return nil, fmt.Errorf("write codex auth file: %w", err)
		}
	}
	cmd.Env = setEnvValue(cmd.Env, "CODEX_HOME", auth.home)
	// Codex exec can prefer CODEX_API_KEY over account login. The resolved file
	// is the single source of auth for this child, regardless of launch mode.
	cmd.Env = slices.DeleteFunc(cmd.Env, func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return key == "CODEX_AUTH" || key == "CODEX_API_KEY" || key == "OPENAI_API_KEY"
	})
	return cmd, nil
}

type codexAuthSource struct {
	home string
	seed []byte // empty when preserving an existing native credential file
}

// codexAuthKind checks the supported native JSON shapes, never server validity.
func codexAuthKind(data []byte) string {
	var auth struct {
		Mode   string `json:"auth_mode"`
		APIKey string `json:"OPENAI_API_KEY"`
		Tokens *struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil {
		return ""
	}
	if (auth.Mode == "" || auth.Mode == "chatgpt") && auth.Tokens != nil && strings.TrimSpace(auth.Tokens.Access) != "" && strings.TrimSpace(auth.Tokens.Refresh) != "" {
		return "account"
	}
	if (auth.Mode == "" || auth.Mode == "apikey") && strings.TrimSpace(auth.APIKey) != "" {
		return "api"
	}
	return ""
}

func codexUserHome(env []string, platform string) string {
	home := strings.TrimSpace(envValue(env, "HOME"))
	if home == "" && platform == "windows" {
		home = strings.TrimSpace(envValue(env, "USERPROFILE"))
	}
	return home
}

func resolveCodexAuth(env []string) (codexAuthSource, error) {
	var candidates []string
	home := strings.TrimSpace(envValue(env, "CODEX_HOME"))
	if home != "" {
		candidates = []string{home}
	} else if userHome := codexUserHome(env, runtime.GOOS); userHome != "" {
		home = filepath.Join(userHome, ".config", "bridgectl", "codex-home")
		candidates = []string{filepath.Join(userHome, ".codex"), home}
	}
	if !filepath.IsAbs(home) {
		return codexAuthSource{}, fmt.Errorf("codex authentication requires an absolute HOME or CODEX_HOME")
	}
	var existingAPI string
	for _, candidate := range candidates {
		data, err := os.ReadFile(filepath.Join(candidate, "auth.json"))
		if err != nil && !os.IsNotExist(err) {
			return codexAuthSource{}, fmt.Errorf("read codex auth file: %w", err)
		}
		switch codexAuthKind(data) {
		case "account":
			return codexAuthSource{home: candidate}, nil
		case "api":
			if existingAPI == "" {
				existingAPI = candidate
			}
		}
	}
	seed := []byte(envValue(env, "CODEX_AUTH"))
	if codexAuthKind(seed) != "" {
		return codexAuthSource{home: home, seed: seed}, nil
	}
	for _, key := range []string{"CODEX_API_KEY", "OPENAI_API_KEY"} {
		if value := strings.TrimSpace(envValue(env, key)); value != "" {
			data, _ := json.Marshal(map[string]string{"OPENAI_API_KEY": value})
			return codexAuthSource{home: home, seed: data}, nil
		}
	}
	if existingAPI != "" {
		return codexAuthSource{home: existingAPI}, nil
	}
	return codexAuthSource{}, fmt.Errorf("codex requires valid account auth.json, CODEX_AUTH JSON, CODEX_API_KEY, or OPENAI_API_KEY; refresh the desktop credentials")
}

func setEnvValue(env []string, key, value string) []string {
	prefix := key + "="
	replacement := prefix + value
	out := append([]string(nil), env...)
	replaced := false
	for i, item := range out {
		if strings.HasPrefix(item, prefix) {
			out[i] = replacement
			replaced = true
		}
	}
	if !replaced {
		out = append(out, replacement)
	}
	return out
}

// atomicWriteFile writes data to a temp file in the same directory as path,
// then renames it into place so readers never see a partial write.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	defer func() { _ = os.Remove(tmpName) }()
	return os.Rename(tmpName, path)
}

// Cleanup leaves desktop-owned auth, helper binaries, and session state intact.
func (p *CodexProvider) Cleanup() {}
