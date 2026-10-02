package provider

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/codexapp"
)

// CodexAppServerProvider runs the real Codex TUI against a companion
// app-server. A session-local protocol relay observes the TUI owner's
// authoritative state and request identities, and can answer a supported
// pending input request using its original JSON-RPC response mechanism.
// Supervisor independently enforces writer ownership before any response.
// See docs/pending-input-response.md for the verified protocol and boundaries.
type CodexAppServerProvider struct {
	*CodexProvider

	mu       sync.Mutex
	sessions map[string]*codexAppServerSession
}

type codexAppServerSession struct {
	port     int
	cmd      *exec.Cmd
	observer *codexapp.ResponseClient
}

// portReadyTimeout bounds how long BuildCommand waits for the companion
// app-server to accept TCP connections before giving up and failing the
// session start outright (never silently falling back to a plain PTY
// session with no interaction capability — that would be exactly the kind
// of unannounced capability downgrade MAR-85 exists to prevent).
const portReadyTimeout = 15 * time.Second

// NewCodexAppServerProvider creates the interaction-observable Codex
// provider. binary/providerRoot mirror NewCodexProvider's construction.
func NewCodexAppServerProvider(cfg StdioConfig) *CodexAppServerProvider {
	cfg.InteractionCapabilities = codexapp.Capabilities
	return &CodexAppServerProvider{
		CodexProvider: NewCodexProvider(cfg),
		sessions:      make(map[string]*codexAppServerSession),
	}
}

func (p *CodexAppServerProvider) PromptPattern() *regexp.Regexp {
	// The TUI's own prompt is unchanged by --remote; reuse the plain
	// provider's pattern rather than inventing a new one.
	return regexp.MustCompile(`(?m)(>\s*$|›)`)
}

// BuildCommand starts the companion `codex app-server` process, waits for
// it to become reachable, and returns the *exec.Cmd for `codex --remote
// ws://127.0.0.1:<port>` — the actual PTY child Supervisor drives exactly
// like any other interactive provider. The companion is torn down when ctx
// (the session's own context) is cancelled, whatever the reason.
func (p *CodexAppServerProvider) BuildCommand(ctx context.Context, cfg bridge.SessionConfig) (*exec.Cmd, error) {
	port, err := freeLocalPort()
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: find a free port: %w", err)
	}
	endpoint := fmt.Sprintf("ws://127.0.0.1:%d", port)

	appServerCmd, tuiCmd, err := p.launchCommands(ctx, cfg, endpoint)
	if err != nil {
		return nil, err
	}
	var logPath string
	if logFile, logErr := os.CreateTemp("", "bridgectl-codex-appserver-*.log"); logErr == nil {
		logPath = logFile.Name()
		appServerCmd.Stdout = logFile
		appServerCmd.Stderr = logFile
		go func() { <-ctx.Done(); _ = logFile.Close() }()
	}
	if err := appServerCmd.Start(); err != nil {
		return nil, fmt.Errorf("codex-app-server: start companion app-server: %w", err)
	}

	if !waitForPort(ctx, "127.0.0.1", port, portReadyTimeout) {
		_ = appServerCmd.Process.Kill()
		_ = appServerCmd.Wait()
		return nil, fmt.Errorf("codex-app-server: companion app-server on %s never became reachable%s", endpoint, companionLogTail(logPath))
	}

	proxyEndpoint, observer, err := codexapp.Proxy(ctx, endpoint)
	if err != nil {
		_ = appServerCmd.Process.Kill()
		_ = appServerCmd.Wait()
		return nil, err
	}
	p.mu.Lock()
	p.sessions[cfg.SessionID] = &codexAppServerSession{port: port, cmd: appServerCmd, observer: observer}
	p.mu.Unlock()
	go func() {
		<-ctx.Done()
		_ = appServerCmd.Process.Kill()
		_ = appServerCmd.Wait()
		p.mu.Lock()
		delete(p.sessions, cfg.SessionID)
		p.mu.Unlock()
	}()

	// Insert before user arguments (which may include a positional prompt,
	// resume/fork subcommand, or -- end-of-options delimiter).
	prefix := codexLauncherPrefix(tuiCmd)
	tuiCmd.Args = append(append(append([]string(nil), tuiCmd.Args[:prefix]...), "--remote", proxyEndpoint), tuiCmd.Args[prefix:]...)
	return tuiCmd, nil
}

func codexLauncherPrefix(cmd *exec.Cmd) int {
	name := strings.TrimSuffix(filepath.Base(cmd.Path), ".exe")
	if (name == "node" || name == "nodejs") && len(cmd.Args) > 1 && !strings.HasPrefix(cmd.Args[1], "-") {
		return 2
	}
	return 1
}

// Preserve the configured executable, launcher script, CLI arguments, options,
// cwd and authentication when enabling the observer for the ordinary provider.
func (p *CodexAppServerProvider) launchCommands(ctx context.Context, cfg bridge.SessionConfig, endpoint string) (*exec.Cmd, *exec.Cmd, error) {
	tui, err := p.CodexProvider.BuildCommand(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	prefix := codexLauncherPrefix(tui)
	args := tui.Args[prefix:]
	companionArgs := append([]string(nil), tui.Args[1:prefix]...)
	companionArgs = append(companionArgs, "app-server", "--listen", endpoint)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if arg == "--remote" || strings.HasPrefix(arg, "--remote=") {
			return nil, nil, fmt.Errorf("codex structured reporting owns --remote; use transport: stdio for an external app server")
		}
		switch {
		case arg == "-c" || arg == "--config" || arg == "--enable" || arg == "--disable":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("codex %s requires a value", arg)
			}
			companionArgs = append(companionArgs, arg, args[i+1])
			i++
		case arg == "--strict-config" || strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "--enable=") || strings.HasPrefix(arg, "--disable="):
			companionArgs = append(companionArgs, arg)
		}
	}
	app := exec.CommandContext(ctx, tui.Path, companionArgs...)
	app.Dir = tui.Dir
	app.Env = append([]string(nil), tui.Env...)
	return app, tui, nil
}

// WatchInteraction implements bridge.InteractionWatcher. It is only ever
// called by Supervisor after BuildCommand has already returned successfully
// for the same sessionID, so the companion's port is always already known.
func (p *CodexAppServerProvider) WatchInteraction(ctx context.Context, sessionID string, _ bridge.SessionConfig) (<-chan bridge.Interaction, error) {
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("codex-app-server: no companion app-server recorded for session %q", sessionID)
	}
	return sess.observer.Updates, nil
}

func (p *CodexAppServerProvider) RespondToInput(ctx context.Context, sessionID, pendingID, text string) error {
	p.mu.Lock()
	sess := p.sessions[sessionID]
	var observer *codexapp.ResponseClient
	if sess != nil {
		observer = sess.observer
	}
	p.mu.Unlock()
	if observer == nil {
		return bridge.ErrRemoteResponseUnsupported
	}
	return observer.Respond(ctx, pendingID, text)
}

func (p *CodexAppServerProvider) DecideApproval(ctx context.Context, sessionID, pendingID, decision string) error {
	p.mu.Lock()
	sess := p.sessions[sessionID]
	p.mu.Unlock()
	if sess == nil {
		return bridge.ErrRemoteResponseUnsupported
	}
	return sess.observer.Decide(ctx, pendingID, decision)
}

// CompanionEndpoint returns the ws:// URL of sessionID's companion
// app-server, for diagnostics (e.g. `bridgectl doctor`) or a caller that
// needs to connect its own additional client to the same instance. Returns
// ok=false once BuildCommand hasn't run yet for this session, or after the
// session has stopped and its companion was torn down.
func (p *CodexAppServerProvider) CompanionEndpoint(sessionID string) (endpoint string, ok bool) {
	p.mu.Lock()
	sess, found := p.sessions[sessionID]
	p.mu.Unlock()
	if !found {
		return "", false
	}
	return fmt.Sprintf("ws://127.0.0.1:%d", sess.port), true
}

// freeLocalPort asks the OS for an unused TCP port by briefly binding to
// port 0 and reading back what was assigned. This is inherently a
// time-of-check/time-of-use race (another process could bind the same port
// before `codex app-server` does) — acceptable for a local, single-host
// development tool; a bind failure simply fails this session start
// cleanly, the same as any other port conflict would.
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// companionLogTail reads back the companion app-server's redirected
// stdout/stderr so a startup failure (e.g. a missing script under
// node_modules, or a syntax/dependency error) is visible in the returned
// error instead of only sitting in a discarded temp file nobody is told
// about.
func companionLogTail(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	const maxLines = 20
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return "\ncompanion app-server output:\n" + strings.Join(lines, "\n")
}

// waitForPort polls until a TCP connection to host:port succeeds, ctx is
// cancelled, or timeout elapses.
func waitForPort(ctx context.Context, host string, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
	}
	return false
}

func (p *CodexAppServerProvider) ObserveActivity(id string, after uint64, events, bytes int) (bridge.ActivityWindow, error) {
	p.mu.Lock()
	sess := p.sessions[id]
	p.mu.Unlock()
	if sess == nil {
		return bridge.ActivityWindow{}, bridge.ErrSessionNotFound
	}
	return sess.observer.Observe(after, events, bytes), nil
}
func (p *CodexAppServerProvider) SendInstruction(ctx context.Context, id, text string) error {
	p.mu.Lock()
	sess := p.sessions[id]
	p.mu.Unlock()
	if sess == nil {
		return bridge.ErrSessionNotFound
	}
	return sess.observer.Instruct(ctx, text)
}
