package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/codexapp"
)

// CodexAppServerProvider is an interactive Codex session (a real, unmodified
// PTY-attached `codex` TUI — the local experience is identical to the plain
// `codex` provider) that additionally gains authoritative interaction state
// by pointing that TUI at an out-of-process `codex app-server` instance
// (`codex --remote ws://127.0.0.1:<port>`) and opening a second, read-only
// observer connection to the same app-server (internal/codexapp.Watch).
//
// The observer never sends turn/start and never answers an approval/input
// request — it only watches ThreadStatus. Every approval/input decision is
// still made by whoever is actually attached to the PTY and typing into the
// real TUI, exactly as with the plain `codex` provider today. This is
// deliberate: MAR-85 is read-only interaction-state observation, not a new
// remote-control path (that's MAR-66, explicitly out of scope here).
//
// See docs/codex-app-server-observer.md for the protocol evidence this was
// built against and the reasoning behind this two-process design.
type CodexAppServerProvider struct {
	*CodexProvider

	mu       sync.Mutex
	sessions map[string]*codexAppServerSession
}

type codexAppServerSession struct {
	port int
	cmd  *exec.Cmd
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
	binPath, err := resolveBinaryPath(p.Binary(), "")
	if err != nil {
		return nil, fmt.Errorf("%w: resolve binary %q: %v", bridge.ErrProviderUnavailable, p.Binary(), err)
	}

	port, err := freeLocalPort()
	if err != nil {
		return nil, fmt.Errorf("codex-app-server: find a free port: %w", err)
	}
	endpoint := fmt.Sprintf("ws://127.0.0.1:%d", port)

	env := cfg.Env
	if env == nil {
		env = FilterEnv(os.Environ())
	} else {
		env = append([]string(nil), env...)
	}

	appServerCmd := exec.CommandContext(ctx, binPath, "app-server", "--listen", endpoint)
	appServerCmd.Dir = cfg.RepoPath
	appServerCmd.Env = env
	if err := applyCodexHomeAuth(appServerCmd); err != nil {
		return nil, err
	}
	if logFile, logErr := os.CreateTemp("", "bridgectl-codex-appserver-*.log"); logErr == nil {
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
		return nil, fmt.Errorf("codex-app-server: companion app-server on %s never became reachable", endpoint)
	}

	p.mu.Lock()
	p.sessions[cfg.SessionID] = &codexAppServerSession{port: port, cmd: appServerCmd}
	p.mu.Unlock()
	go func() {
		<-ctx.Done()
		_ = appServerCmd.Process.Kill()
		_ = appServerCmd.Wait()
		p.mu.Lock()
		delete(p.sessions, cfg.SessionID)
		p.mu.Unlock()
	}()

	tuiCmd := exec.CommandContext(ctx, binPath, "--remote", endpoint)
	tuiCmd.Dir = cfg.RepoPath
	tuiCmd.Env = append([]string(nil), env...)
	if err := applyCodexHomeAuth(tuiCmd); err != nil {
		return nil, err
	}
	return tuiCmd, nil
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
	endpoint := fmt.Sprintf("ws://127.0.0.1:%d", sess.port)
	return codexapp.Watch(ctx, endpoint, slog.Default())
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
