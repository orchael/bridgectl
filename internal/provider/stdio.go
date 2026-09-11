package provider

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/orchael/bridgectl/internal/bridge"
)

// StdioConfig configures an interactive PTY-backed provider.
type StdioConfig struct {
	ProviderID     string
	Binary         string
	DefaultArgs    []string
	StartupTimeout time.Duration
	StopGrace      time.Duration
	StartupProbe   string
	PromptPattern  string
	RequiredEnv    []string
	StreamJSON     bool // if true, the provider uses stream-JSON mode (no PTY)
	StripANSI      bool // if true, ANSI escape codes are stripped from PTY output
	// ProviderRoot is an optional absolute path used as the base for resolving
	// relative Binary and DefaultArgs paths. When empty, relative paths are
	// resolved against the daemon working directory (legacy behaviour).
	ProviderRoot string
}

// StdioProvider defines how to launch and validate one interactive CLI.
type StdioProvider struct {
	cfg            StdioConfig
	promptRe       *regexp.Regexp
	mu             sync.RWMutex
	unavailableErr error
}

// SetUnavailable persists a startup-time error so that Health() reports the
// provider as unavailable until the process restarts. Used by the daemon to
// propagate startup-probe failures into the runtime health state.
func (p *StdioProvider) SetUnavailable(err error) {
	p.mu.Lock()
	p.unavailableErr = err
	p.mu.Unlock()
}

func NewStdioProvider(cfg StdioConfig) *StdioProvider {
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 45 * time.Second
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 10 * time.Second
	}
	if cfg.StartupProbe == "" {
		cfg.StartupProbe = "prompt"
	}
	p := &StdioProvider{cfg: cfg}
	if cfg.PromptPattern != "" {
		p.promptRe = regexp.MustCompile(cfg.PromptPattern)
	}
	return p
}

func (p *StdioProvider) ID() string                    { return p.cfg.ProviderID }
func (p *StdioProvider) Binary() string                { return p.cfg.Binary }
func (p *StdioProvider) PromptPattern() *regexp.Regexp { return p.promptRe }
func (p *StdioProvider) StartupTimeout() time.Duration { return p.cfg.StartupTimeout }
func (p *StdioProvider) StopGrace() time.Duration      { return p.cfg.StopGrace }

// IsStreamJSON implements bridge.StreamJSONProvider. It returns true when the
// provider is configured with StreamJSON: true (i.e. it emits JSONL on stdout
// instead of raw PTY bytes).
func (p *StdioProvider) IsStreamJSON() bool { return p.cfg.StreamJSON }

// IsStripANSI implements bridge.StripANSIProvider. It returns true when the
// provider is configured with StripANSI: true so the supervisor strips ANSI
// escape codes from PTY output before forwarding to clients.
func (p *StdioProvider) IsStripANSI() bool { return p.cfg.StripANSI }

func (p *StdioProvider) BuildCommand(ctx context.Context, cfg bridge.SessionConfig) (*exec.Cmd, error) {
	binPath, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve binary %q: %v", bridge.ErrProviderUnavailable, p.cfg.Binary, err)
	}
	args, err := commandArgsForProvider(p.cfg.ProviderID, p.cfg.DefaultArgs, p.cfg.ProviderRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve args for %q: %v", bridge.ErrProviderUnavailable, p.cfg.ProviderID, err)
	}
	for key, value := range cfg.Options {
		if strings.HasPrefix(key, "arg:") {
			args = append(args, value)
		}
	}
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = cfg.RepoPath
	if cfg.Env != nil {
		cmd.Env = append([]string(nil), cfg.Env...)
	} else {
		cmd.Env = FilterEnv(os.Environ())
	}
	return cmd, nil
}

func (p *StdioProvider) ValidateStartup(ctx context.Context) error {
	return p.validateStartupWithEnv(ctx, filterEnv(os.Environ()))
}

func (p *StdioProvider) validateStartupWithEnv(ctx context.Context, env []string) error {
	for _, envName := range p.cfg.RequiredEnv {
		if strings.TrimSpace(envValue(env, envName)) == "" {
			return fmt.Errorf("provider %q requires env var %q", p.cfg.ProviderID, envName)
		}
	}
	if p.promptRe == nil {
		if p.cfg.StartupProbe == "prompt" {
			return nil
		}
	}
	switch p.cfg.StartupProbe {
	case "none":
		return nil
	case "output":
		return p.validateStartupOutput(ctx, env)
	case "prompt":
		return p.validateStartupPrompt(ctx, env)
	default:
		return fmt.Errorf("provider %q has unsupported startup probe %q", p.cfg.ProviderID, p.cfg.StartupProbe)
	}
}

func (p *StdioProvider) validateStartupPrompt(ctx context.Context, env []string) error {
	if p.promptRe == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.StartupTimeout)
	defer cancel()

	binPath, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return err
	}
	args, err := commandArgsForProvider(p.cfg.ProviderID, p.cfg.DefaultArgs, p.cfg.ProviderRoot)
	if err != nil {
		return err
	}
	wd, _ := os.Getwd()
	cmd := exec.CommandContext(probeCtx, binPath, args...)
	cmd.Dir = wd
	cmd.Env = env

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		return fmt.Errorf("provider %q startup probe: %w", p.cfg.ProviderID, err)
	}
	defer func() {
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
	}()

	buf := make([]byte, 4096)
	var seen bytes.Buffer
	for {
		if probeCtx.Err() != nil {
			return fmt.Errorf("provider %q startup probe timed out waiting for prompt %q; output:\n%s", p.cfg.ProviderID, p.cfg.PromptPattern, seen.String())
		}
		_ = ptmx.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, readErr := ptmx.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			if p.promptRe.Match(seen.Bytes()) {
				return nil
			}
		}
		if readErr != nil {
			if os.IsTimeout(readErr) {
				continue
			}
			return fmt.Errorf("provider %q startup probe failed: %v; output:\n%s", p.cfg.ProviderID, readErr, seen.String())
		}
	}
}

func (p *StdioProvider) validateStartupOutput(ctx context.Context, env []string) error {
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.StartupTimeout)
	defer cancel()

	binPath, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return err
	}
	args, err := commandArgsForProvider(p.cfg.ProviderID, p.cfg.DefaultArgs, p.cfg.ProviderRoot)
	if err != nil {
		return err
	}
	wd, _ := os.Getwd()
	cmd := exec.CommandContext(probeCtx, binPath, args...)
	cmd.Dir = wd
	cmd.Env = env

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		return fmt.Errorf("provider %q startup probe: %w", p.cfg.ProviderID, err)
	}
	defer func() {
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
	}()

	buf := make([]byte, 4096)
	var seen bytes.Buffer
	for {
		if probeCtx.Err() != nil {
			return fmt.Errorf("provider %q startup probe timed out waiting for output; output:\n%s", p.cfg.ProviderID, seen.String())
		}
		_ = ptmx.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, readErr := ptmx.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			slog.Debug("provider startup probe output", "provider", p.cfg.ProviderID, "bytes", n)
			time.Sleep(250 * time.Millisecond)
			return nil
		}
		if readErr != nil {
			if os.IsTimeout(readErr) {
				continue
			}
			return fmt.Errorf("provider %q startup probe failed: %v; output:\n%s", p.cfg.ProviderID, readErr, seen.String())
		}
	}
}

func (p *StdioProvider) Version(ctx context.Context) (string, error) {
	path, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return "", fmt.Errorf("binary %q not found: %w", p.cfg.Binary, err)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	// Use a minimal environment for version probes. Passing auth tokens (e.g.
	// CLAUDE_CODE_OAUTH_TOKEN) causes some provider binaries to make network
	// round-trips to validate credentials before printing their version, which
	// can exceed the startup timeout.
	cmd.Env = versionProbeEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("version check: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *StdioProvider) Health(ctx context.Context) error {
	return p.HealthWithEnv(ctx, FilterEnv(os.Environ()))
}

func (p *StdioProvider) HealthWithEnv(ctx context.Context, env []string) error {
	p.mu.RLock()
	unavailErr := p.unavailableErr
	p.mu.RUnlock()
	if unavailErr != nil {
		return unavailErr
	}
	if err := validateProviderUnprotectedEnv(p.cfg.ProviderID, env); err != nil {
		return err
	}
	path, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return fmt.Errorf("binary %q not found: %w", p.cfg.Binary, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("binary %q is not executable", path)
	}
	for _, envName := range p.cfg.RequiredEnv {
		if strings.TrimSpace(envValue(env, envName)) == "" {
			return fmt.Errorf("required env var %s not set", envName)
		}
	}
	return nil
}

// absRoot returns root as an absolute path. If root is already absolute it is
// returned unchanged. If root is relative it is made absolute relative to the
// process working directory. This ensures that binary and arg paths joined
// against root do not accidentally resolve relative to cmd.Dir (the session
// repo path) when the process working directory differs.
func absRoot(root string) (string, error) {
	if filepath.IsAbs(root) {
		return root, nil
	}
	return filepath.Abs(root)
}

// resolveBinaryPath resolves a provider binary to an absolute path. When root
// is non-empty and binary is a relative path containing a slash, binary is
// resolved relative to root instead of the process working directory.
func resolveBinaryPath(binary, root string) (string, error) {
	if strings.Contains(binary, "/") {
		if filepath.IsAbs(binary) {
			return binary, nil
		}
		if root != "" {
			absR, err := absRoot(root)
			if err != nil {
				return "", fmt.Errorf("absolutize provider root: %w", err)
			}
			return filepath.Join(absR, binary), nil
		}
		return filepath.Abs(binary)
	}
	return exec.LookPath(binary)
}

// resolveCommandArgs converts standalone relative path arguments to absolute
// paths. Only bare path arguments (e.g. "./foo" or "../foo") are rewritten;
// command names resolved via PATH and embedded flag values (e.g.
// "--config=./foo") are left unchanged. When root is non-empty, relative paths
// are resolved against root instead of the process working directory.
func resolveCommandArgs(args []string, root string) ([]string, error) {
	if len(args) == 0 {
		return nil, nil
	}

	resolved := append([]string(nil), args...)
	for i, arg := range resolved {
		if !isStandaloneRelativePathArg(arg) {
			continue
		}
		var abs string
		var err error
		if root != "" {
			absR, err := absRoot(root)
			if err != nil {
				return nil, fmt.Errorf("absolutize provider root: %w", err)
			}
			abs = filepath.Join(absR, arg)
		} else {
			abs, err = filepath.Abs(arg)
			if err != nil {
				return nil, err
			}
		}
		resolved[i] = abs
	}
	return resolved, nil
}

func isStandaloneRelativePathArg(arg string) bool {
	if arg == "." || arg == ".." {
		return true
	}
	return strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../")
}

type unprotectedModeConfig struct {
	envVar string
	args   []string
}

var providerUnprotectedModes = map[string]unprotectedModeConfig{
	"codex": {
		envVar: "BRIDGE_CODEX_UNPROTECTED",
		args:   []string{"--dangerously-bypass-approvals-and-sandbox"},
	},
	"claude": {
		envVar: "BRIDGE_CLAUDE_UNPROTECTED",
		args:   []string{"--dangerously-skip-permissions"},
	},
	"opencode": {
		envVar: "BRIDGE_OPENCODE_UNPROTECTED",
		args:   []string{"--auto"},
	},
	"gemini": {
		envVar: "BRIDGE_GEMINI_UNPROTECTED",
		args:   []string{"--yolo"},
	},
}

func commandArgsForProvider(providerID string, args []string, root string) ([]string, error) {
	resolved, err := resolveCommandArgs(args, root)
	if err != nil {
		return nil, err
	}

	mode, ok := providerUnprotectedModes[providerID]
	if !ok {
		return resolved, nil
	}

	enabled, err := parseProviderUnprotectedMode(providerID, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return resolved, nil
	}
	return append(resolved, mode.args...), nil
}

func validateProviderUnprotectedEnv(providerID string, env []string) error {
	_, err := parseProviderUnprotectedMode(providerID, func(key string) (string, bool) {
		value := envValue(env, key)
		return value, strings.TrimSpace(value) != ""
	})
	return err
}

func parseProviderUnprotectedMode(providerID string, lookup func(string) (string, bool)) (bool, error) {
	mode, ok := providerUnprotectedModes[providerID]
	if !ok {
		return false, nil
	}
	raw, ok := lookup(mode.envVar)
	if !ok || strings.TrimSpace(raw) == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", mode.envVar, err)
	}
	return enabled, nil
}

// versionProbeEnv returns a minimal environment for --version checks.
// It deliberately excludes auth tokens and API keys so that provider binaries
// that make network calls when credentials are present (e.g. token validation)
// do not time out during the startup version probe.
func versionProbeEnv() []string {
	// Use the parent PATH when set; fall back to a safe minimal default so
	// that binaries can still locate their dynamic linker and helpers.
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"PATH=" + path,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	}
	// Only set HOME when non-empty; leaving it unset lets the OS fall back to
	// the passwd database, which is preferable to forcing an empty value that
	// can confuse CLIs expecting a writable home directory.
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	} else if home, err := os.UserHomeDir(); err == nil {
		env = append(env, "HOME="+home)
	}
	// Preserve NO_COLOR and temp-dir hints if set.
	for _, key := range []string{"NO_COLOR", "TMPDIR", "TMP", "TEMP"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// FilterEnv returns a filtered environment excluding sensitive variables and
// variables that interfere with subprocess behaviour.
func FilterEnv(env []string) []string {
	blocked := map[string]bool{
		"AWS_SECRET_ACCESS_KEY": true,
		"AWS_SESSION_TOKEN":     true,
		"SLACK_BOT_TOKEN":       true,
		"SLACK_SIGNING_SECRET":  true,
		"DISCORD_TOKEN":         true,
		"CLAUDECODE":            true,
	}
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		key, _, ok := strings.Cut(e, "=")
		if ok && blocked[key] {
			continue
		}
		filtered = append(filtered, e)
	}
	if !hasEnvKey(filtered, "TERM") {
		filtered = append(filtered, "TERM=xterm-256color")
	}
	if !hasEnvKey(filtered, "COLORTERM") {
		filtered = append(filtered, "COLORTERM=truecolor")
	}
	return filtered
}

func filterEnv(env []string) []string {
	return FilterEnv(env)
}

func envValue(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			value = strings.TrimPrefix(item, prefix)
		}
	}
	return value
}

func hasEnvKey(env []string, key string) bool {
	for _, e := range env {
		k, _, ok := strings.Cut(e, "=")
		if ok && k == key {
			return true
		}
	}
	return false
}
