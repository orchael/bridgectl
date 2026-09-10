package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

// OpenCodeServerConfig configures the headless OpenCode server-mode provider.
// The bridge launches `opencode serve` on a localhost port and communicates
// with the OpenCode HTTP/SSE API instead of driving a PTY.
type OpenCodeServerConfig struct {
	ProviderID     string
	Binary         string
	DefaultArgs    []string
	StartupTimeout time.Duration
	StopGrace      time.Duration
	RequiredEnv    []string
	// Hostname to bind the OpenCode server to. Defaults to "127.0.0.1".
	Hostname string
	// PortRangeStart is the beginning of the port range for allocating server
	// ports. Defaults to 4100.
	PortRangeStart int
	// PortRangeEnd is the end of the port range (exclusive). Defaults to 4200.
	PortRangeEnd int
	// ProviderRoot is an optional absolute path used as the base for resolving
	// relative binary paths. When empty, relative paths are resolved against
	// the daemon working directory.
	ProviderRoot string
}

// OpenCodeServerProvider implements bridge.Provider for the OpenCode server
// mode. Instead of a PTY, it launches `opencode serve` as a subprocess and
// exposes a structured HTTP/SSE interface.
//
// For v1, the provider launches one `opencode serve` process per session via
// BuildCommand. The returned *exec.Cmd runs `opencode serve` bound to a
// localhost port. The supervisor still manages the subprocess lifecycle, but
// the provider advertises itself as a StreamJSON provider so the supervisor
// reads structured JSONL from stdout instead of raw PTY bytes.
//
// The provider resolves a free localhost port at BuildCommand time and injects
// the --hostname and --port flags. The OpenCode server's HTTP API base URL is
// stored per-session in the provider for future use by message-oriented RPCs
// (SendMessage, AbortTurn, etc. -- planned in a later phase).
type OpenCodeServerProvider struct {
	cfg OpenCodeServerConfig

	mu             sync.RWMutex
	unavailableErr error
	// sessions tracks per-session metadata (port, base URL, etc.).
	sessions map[string]*openCodeSessionMeta
}

// openCodeSessionMeta holds runtime metadata for one OpenCode server session.
type openCodeSessionMeta struct {
	Port    int
	BaseURL string
}

// NewOpenCodeServerProvider creates a headless OpenCode server-mode provider.
func NewOpenCodeServerProvider(cfg OpenCodeServerConfig) *OpenCodeServerProvider {
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 30 * time.Second
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 10 * time.Second
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "127.0.0.1"
	}
	if cfg.PortRangeStart <= 0 {
		cfg.PortRangeStart = 4100
	}
	if cfg.PortRangeEnd <= 0 {
		cfg.PortRangeEnd = 4200
	}
	return &OpenCodeServerProvider{
		cfg:      cfg,
		sessions: make(map[string]*openCodeSessionMeta),
	}
}

func (p *OpenCodeServerProvider) ID() string                    { return p.cfg.ProviderID }
func (p *OpenCodeServerProvider) Binary() string                { return p.cfg.Binary }
func (p *OpenCodeServerProvider) PromptPattern() *regexp.Regexp { return nil }
func (p *OpenCodeServerProvider) StartupTimeout() time.Duration { return p.cfg.StartupTimeout }
func (p *OpenCodeServerProvider) StopGrace() time.Duration      { return p.cfg.StopGrace }

// IsStreamJSON returns true. The OpenCode server provider emits structured
// output on stdout that the supervisor should parse as JSONL.
func (p *OpenCodeServerProvider) IsStreamJSON() bool { return true }

// SetUnavailable persists a startup-time error so that Health() reports the
// provider as unavailable.
func (p *OpenCodeServerProvider) SetUnavailable(err error) {
	p.mu.Lock()
	p.unavailableErr = err
	p.mu.Unlock()
}

// BuildCommand constructs the `opencode serve` command for a session. It
// allocates a free localhost port, constructs the server flags, and returns
// the exec.Cmd. The supervisor owns the subprocess lifecycle.
func (p *OpenCodeServerProvider) BuildCommand(ctx context.Context, cfg bridge.SessionConfig) (*exec.Cmd, error) {
	binPath, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve binary %q: %v", bridge.ErrProviderUnavailable, p.cfg.Binary, err)
	}

	port, err := p.allocatePort()
	if err != nil {
		return nil, fmt.Errorf("allocate port for opencode-server session: %w", err)
	}

	args, err := resolveCommandArgs(p.cfg.DefaultArgs, p.cfg.ProviderRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve args for %q: %v", bridge.ErrProviderUnavailable, p.cfg.ProviderID, err)
	}

	// Append the `serve` subcommand and server flags.
	args = append(args, "serve",
		"--hostname", p.cfg.Hostname,
		"--port", fmt.Sprintf("%d", port),
	)

	// Append any session-specific arg:* options.
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

	baseURL := "http://" + net.JoinHostPort(p.cfg.Hostname, fmt.Sprintf("%d", port))

	p.mu.Lock()
	p.sessions[cfg.SessionID] = &openCodeSessionMeta{
		Port:    port,
		BaseURL: baseURL,
	}
	p.mu.Unlock()

	slog.Info("opencode-server: allocated session",
		"session_id", cfg.SessionID,
		"port", port,
		"base_url", baseURL,
	)

	return cmd, nil
}

// ValidateStartup checks that required environment variables are set.
func (p *OpenCodeServerProvider) ValidateStartup(ctx context.Context) error {
	for _, envName := range p.cfg.RequiredEnv {
		if strings.TrimSpace(os.Getenv(envName)) == "" {
			return fmt.Errorf("provider %q requires env var %q", p.cfg.ProviderID, envName)
		}
	}
	return nil
}

// Version runs `<binary> --version` to report the provider version.
func (p *OpenCodeServerProvider) Version(ctx context.Context) (string, error) {
	path, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return "", fmt.Errorf("binary %q not found: %w", p.cfg.Binary, err)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = versionProbeEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("version check: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Health checks that the binary exists, is executable, and required env is set.
func (p *OpenCodeServerProvider) Health(ctx context.Context) error {
	return p.HealthWithEnv(ctx, FilterEnv(os.Environ()))
}

// HealthWithEnv checks health using the provided environment.
func (p *OpenCodeServerProvider) HealthWithEnv(ctx context.Context, env []string) error {
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

// WaitForHealth polls the OpenCode server's health endpoint until it responds
// with 200 or the context is cancelled. This should be called after the
// subprocess is started and before sending any API requests.
func (p *OpenCodeServerProvider) WaitForHealth(ctx context.Context, sessionID string) error {
	p.mu.RLock()
	meta, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("opencode-server: no session metadata for %q", sessionID)
	}

	healthURL := meta.BaseURL + "/global/health"
	client := &http.Client{Timeout: 2 * time.Second}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("opencode-server: health check timed out for session %q: %w", sessionID, ctx.Err())
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				slog.Info("opencode-server: health check passed",
					"session_id", sessionID,
					"base_url", meta.BaseURL,
				)
				return nil
			}
		}
	}
}

// SessionBaseURL returns the HTTP base URL for a running OpenCode server
// session, or an error if the session is not tracked.
func (p *OpenCodeServerProvider) SessionBaseURL(sessionID string) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	meta, ok := p.sessions[sessionID]
	if !ok {
		return "", fmt.Errorf("opencode-server: no session metadata for %q", sessionID)
	}
	return meta.BaseURL, nil
}

// CleanupSession removes tracked session metadata. Should be called when the
// session is stopped.
func (p *OpenCodeServerProvider) CleanupSession(sessionID string) {
	p.mu.Lock()
	delete(p.sessions, sessionID)
	p.mu.Unlock()
}

// allocatePort finds a free TCP port in the configured range by attempting to
// listen on each port. Ports already assigned to tracked sessions are skipped
// to avoid intra-process collisions. If no port in the range is free, it falls
// back to letting the OS pick a port.
func (p *OpenCodeServerProvider) allocatePort() (int, error) {
	// Collect ports already assigned to active sessions.
	p.mu.RLock()
	usedPorts := make(map[int]bool, len(p.sessions))
	for _, meta := range p.sessions {
		usedPorts[meta.Port] = true
	}
	p.mu.RUnlock()

	for port := p.cfg.PortRangeStart; port < p.cfg.PortRangeEnd; port++ {
		if usedPorts[port] {
			continue
		}
		addr := net.JoinHostPort(p.cfg.Hostname, fmt.Sprintf("%d", port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	// Fallback: let the OS pick a free port.
	ln, err := net.Listen("tcp", net.JoinHostPort(p.cfg.Hostname, "0"))
	if err != nil {
		return 0, fmt.Errorf("no free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// SendPrompt sends a user prompt to an active OpenCode server session.
// This is a placeholder for v1; the full implementation will use the OpenCode
// session and message APIs.
//
// TODO(#108): Implement full prompt/message sending via OpenCode HTTP API
// once the bridge proto adds SendMessage/AbortTurn RPCs.
func (p *OpenCodeServerProvider) SendPrompt(ctx context.Context, sessionID, prompt string) error {
	p.mu.RLock()
	meta, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("opencode-server: no session metadata for %q", sessionID)
	}

	// POST to the OpenCode session prompt endpoint.
	// The exact path depends on the OpenCode server API; using the documented
	// pattern from the issue.
	promptURL := meta.BaseURL + "/session/prompt"
	payload := struct {
		Content string `json:"content"`
	}{Content: prompt}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("opencode-server: marshal prompt payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, promptURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return fmt.Errorf("opencode-server: create prompt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("opencode-server: send prompt: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opencode-server: prompt failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// AbortTurn sends an abort request to the active OpenCode server session.
//
// TODO(#108): Wire into bridge AbortTurn RPC when proto additions land.
func (p *OpenCodeServerProvider) AbortTurn(ctx context.Context, sessionID string) error {
	p.mu.RLock()
	meta, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("opencode-server: no session metadata for %q", sessionID)
	}

	abortURL := meta.BaseURL + "/session/abort"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, abortURL, nil)
	if err != nil {
		return fmt.Errorf("opencode-server: create abort request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("opencode-server: abort: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opencode-server: abort failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SubscribeSSE connects to the OpenCode server's SSE event stream and sends
// parsed events to the returned channel. The caller should cancel the context
// to stop the subscription.
//
// TODO(#108): Map OpenCode SSE events into bridge OutputChunk types once the
// event schema is finalized.
func (p *OpenCodeServerProvider) SubscribeSSE(ctx context.Context, sessionID string) (<-chan SSEEvent, error) {
	p.mu.RLock()
	meta, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("opencode-server: no session metadata for %q", sessionID)
	}

	eventURL := meta.BaseURL + "/event"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventURL, nil)
	if err != nil {
		return nil, fmt.Errorf("opencode-server: create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 0} // no timeout for streaming
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode-server: connect SSE: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("opencode-server: SSE returned status %d", resp.StatusCode)
	}

	ch := make(chan SSEEvent, 64)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		parseSSEStream(ctx, resp.Body, ch)
	}()

	return ch, nil
}

// SSEEvent represents a parsed Server-Sent Event from the OpenCode server.
type SSEEvent struct {
	Event string
	Data  string
	ID    string
}

// parseSSEStream reads an SSE stream and sends parsed events to ch.
// sseMaxTokenSize is the maximum line length the SSE scanner will accept.
// The default bufio.Scanner buffer is 64 KB which can silently truncate
// large SSE data payloads. 1 MB accommodates the large JSON events that
// OpenCode emits for file diffs and tool results.
const sseMaxTokenSize = 1024 * 1024 // 1 MB

func parseSSEStream(ctx context.Context, r io.Reader, ch chan<- SSEEvent) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), sseMaxTokenSize)
	var event SSEEvent
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			event.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case strings.HasPrefix(line, "id:"):
			event.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case line == "":
			// Empty line signals end of an event.
			if event.Event != "" || event.Data != "" {
				select {
				case ch <- event:
				case <-ctx.Done():
					return
				}
			}
			event = SSEEvent{}
		}
	}
}

// OpenCodeServerCapabilities returns the capability set for the server-mode
// provider. This is used for provider capability discovery so clients can
// choose the appropriate UI.
func OpenCodeServerCapabilities() []string {
	return []string{
		"message_input",
		"structured_events",
		// TODO(#108): Add "permissions", "files", "diffs" capabilities as
		// bridge proto event types are added.
	}
}
