package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/localserver"
)

// detachKey is ctrl-] (0x1d), used to detach from a session without stopping it.
const detachKey = 0x1d

const maxProviderErrorTail = 16 * 1024

func newRunCmd() *cobra.Command {
	var (
		providerName string
		project      string
		timeout      time.Duration
		noTTY        bool
	)

	cmd := &cobra.Command{
		Use:        "run [directory]",
		Short:      "Start an AI agent session in a directory (deprecated: use 'session start')",
		Deprecated: "use 'bridgectl session start' instead.",
		Long: `Start a local bridge server (if not already running), create a new
session with the specified provider, and attach your terminal.

DEPRECATED: This command is deprecated and will be removed in a future release.
Use 'bridgectl session start' instead, which also supports --remote for
connecting to remote bridge servers.

Press ctrl-] to detach from the session without stopping it.
Use 'bridgectl session attach <id>' to reattach later.

Use --no-tty to run without a terminal, reading from stdin and writing to
stdout. Useful for scripting, piping input, and automated tests.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("resolve directory: %w", err)
			}
			info, err := os.Stat(absDir)
			if err != nil {
				return fmt.Errorf("directory %q: %w", absDir, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", absDir)
			}
			if noTTY {
				return runSessionNoTTY(absDir, providerName, project, timeout)
			}
			return runSession(absDir, providerName, project, timeout)
		},
	}

	cmd.Flags().StringVarP(&providerName, "provider", "p", "claude", "AI provider (claude, codex, opencode, gemini, echo)")
	cmd.Flags().StringVar(&project, "project", "local", "project ID")
	cmd.Flags().DurationVarP(&timeout, "timeout", "t", 30*time.Minute, "session timeout")
	cmd.Flags().BoolVar(&noTTY, "no-tty", false, "run without a terminal (for scripting and tests)")

	return cmd
}

func runSession(dir, providerName, project string, timeout time.Duration) error {
	// Validate terminal before starting a session to avoid orphaning a
	// provider process when stdin is not interactive.
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}

	// Ensure a server is running (spawns a local-mode background process if needed).
	if err := ensureServer(); err != nil {
		return err
	}

	// Connect using auto-detected mode (local or secure).
	client, err := connectClient("", timeout)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	client.SetProject(project)

	cols, rows := currentTTYSize()
	sessionID := uuid.NewString()
	// WithCancel (not WithTimeout): the streaming attach has no natural wall-clock
	// limit. timeout still bounds unary RPCs (StartSession, etc.) via connectClient.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   project,
		SessionId:   sessionID,
		RepoPath:    dir,
		Provider:    providerName,
		InitialCols: cols,
		InitialRows: rows,
	}); err != nil {
		return formatStartSessionError(err)
	}

	// Put terminal in raw mode.
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("set raw terminal: %w", err)
	}
	var restoreOnce sync.Once
	restore := func() {
		restoreOnce.Do(func() {
			_ = term.Restore(fd, oldState)
		})
	}
	defer restore()

	stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  uuid.NewString(),
		AfterSeq:  0,
	})
	if err != nil {
		restore()
		return fmt.Errorf("attach session: %w", err)
	}

	var detached atomic.Bool
	var outputTail strings.Builder

	// Handle signals.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// SIGWINCH handling (resize) — only on unix.
	setupSigwinch(sigCh)

	go func() {
		for sig := range sigCh {
			if isSigwinch(sig) {
				c, r := currentTTYSize()
				_, _ = client.ResizeSession(context.Background(), &bridgev1.ResizeSessionRequest{
					SessionId: sessionID,
					ClientId:  stream.ClientID(),
					Cols:      c,
					Rows:      r,
				})
				continue
			}
			// SIGINT/SIGTERM → stop session and let RecvAll unwind.
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = client.StopSession(stopCtx, &bridgev1.StopSessionRequest{
				SessionId: sessionID,
				Force:     true,
			})
			stopCancel()
			cancel()
			return
		}
	}()

	// Forward stdin → session, watching for detach key.
	// Keystrokes are coalesced into batched WriteInput RPCs to avoid
	// hitting per-session rate limits when typing quickly.
	go func() {
		w := &inputWriter{cfg: inputWriterConfig{
			Reader:    os.Stdin,
			Client:    client,
			SessionID: sessionID,
			ClientID:  stream.ClientID(),
			DetachKey: detachKey,
			OnDetach: func() {
				detached.Store(true)
				cancel()
			},
			OnReadError: func(err error) {
				if err != io.EOF {
					fmt.Fprintf(os.Stderr, "\r\nstdin read failed: %v\r\n", err)
				}
			},
		}}
		w.Run()
	}()

	// Receive session output → stdout.
	err = stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
		switch ev.Type {
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
			if err := codexOutputAuthExpiredError(providerName, &outputTail, ev.Payload); err != nil {
				return err
			}
			_, writeErr := os.Stdout.Write(ev.Payload)
			return writeErr
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP:
			_, writeErr := fmt.Fprintf(os.Stderr, "\r\n[bridgectl] replay gap: oldest=%d last=%d\r\n", ev.OldestSeq, ev.LastSeq)
			return writeErr
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR:
			if err := codexAuthExpiredError(providerName, ev.Error); err != nil {
				return err
			}
			return errors.New(ev.Error)
		default:
			return nil
		}
	})
	restore()

	if detached.Load() {
		fmt.Fprintf(os.Stderr, "\r\nDetached from session %s\r\n", sessionID)
		fmt.Fprintf(os.Stderr, "Reattach with: bridgectl session attach %s\r\n", sessionID)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "\r\nsession ended: %v\r\n", err)
	}
	return nil
}

// ensureServer ensures a bridge server is running. If none is found, it spawns
// "bridgectl server start" as a background process in local mode and
// waits for it to become healthy. For secure mode, the user must start the
// server explicitly with --listen.
func ensureServer() error {
	// Check for existing server (local or secure).
	target, _ := localserver.DiscoverTarget("")
	if target != "" {
		return nil
	}
	if err := secureServerDiscoveryError(); err != nil {
		return err
	}

	// Find our own binary to spawn the server (local mode only).
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	cmd := exec.Command(self, "server", "start")
	setDetachedProcess(cmd)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server process: %w", err)
	}
	// Detach — don't wait for the child.
	go func() { _ = cmd.Wait() }()

	// Poll until the server is healthy.
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		target, _ = localserver.DiscoverTarget("")
		if target != "" {
			return nil
		}
	}
	return fmt.Errorf("server did not start within 5s")
}

func secureServerDiscoveryError() error {
	stateDir := localserver.StateDir()
	if localserver.DiscoverMode(stateDir) != localserver.ModeSecure {
		return nil
	}
	addrData, err := os.ReadFile(filepath.Join(stateDir, "server.addr"))
	if err != nil {
		return nil
	}
	addr := strings.TrimSpace(string(addrData))
	if addr == "" {
		return nil
	}
	return fmt.Errorf("secure bridgectl server is recorded at %s, but the health probe failed; check the user service and credentials under %s/certs", addr, stateDir)
}

// runSessionNoTTY runs a session without a terminal, forwarding raw stdin to
// the provider and writing output to stdout. Used for scripting, piping, and
// automated tests (e.g. the echo provider in CI).
func runSessionNoTTY(dir, providerName, project string, timeout time.Duration) error {
	if err := ensureServer(); err != nil {
		return err
	}

	client, err := connectClient("", timeout)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	client.SetProject(project)

	sessionID := uuid.NewString()
	clientID := uuid.NewString()
	// WithCancel (not WithTimeout): the streaming attach has no natural wall-clock
	// limit. timeout still bounds unary RPCs (StartSession, etc.) via connectClient.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   project,
		SessionId:   sessionID,
		RepoPath:    dir,
		Provider:    providerName,
		InitialCols: 80,
		InitialRows: 24,
	}); err != nil {
		return formatStartSessionError(err)
	}

	stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  clientID,
		AfterSeq:  0,
	})
	if err != nil {
		return fmt.Errorf("attach session: %w", err)
	}

	// attached is closed when the server confirms the session is live and
	// ready to route WriteInput calls (ms.attachedClient is set server-side
	// before the ATTACHED event is sent).
	attached := make(chan struct{})
	var attachOnce sync.Once
	var outputTail strings.Builder

	// Forward stdin to session once attached; stop session on EOF.
	go func() {
		select {
		case <-attached:
		case <-ctx.Done():
			return
		}
		w := &inputWriter{cfg: inputWriterConfig{
			Reader:    os.Stdin,
			Client:    client,
			SessionID: sessionID,
			ClientID:  stream.ClientID(),
			OnReadError: func(_ error) {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, _ = client.StopSession(stopCtx, &bridgev1.StopSessionRequest{
					SessionId: sessionID,
					Force:     true,
				})
				stopCancel()
				cancel()
			},
		}}
		w.Run()
	}()

	err = stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
		if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED {
			attachOnce.Do(func() { close(attached) })
			return nil
		}
		switch ev.Type {
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
			if err := codexOutputAuthExpiredError(providerName, &outputTail, ev.Payload); err != nil {
				return err
			}
			_, writeErr := os.Stdout.Write(ev.Payload)
			return writeErr
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR:
			if err := codexAuthExpiredError(providerName, ev.Error); err != nil {
				return err
			}
			return errors.New(ev.Error)
		default:
			return nil
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("session ended: %w", err)
	}
	return nil
}

func appendProviderOutputTail(tail *strings.Builder, payload []byte) {
	if len(payload) == 0 {
		return
	}
	tail.Write(payload)
	if tail.Len() <= maxProviderErrorTail {
		return
	}
	text := tail.String()
	if len(text) <= maxProviderErrorTail {
		return
	}
	tail.Reset()
	tail.WriteString(text[len(text)-maxProviderErrorTail:])
}

func codexOutputAuthExpiredError(providerName string, tail *strings.Builder, payload []byte) error {
	if providerName != "codex" {
		return nil
	}
	appendProviderOutputTail(tail, payload)
	return codexAuthExpiredError(providerName, tail.String())
}

func codexAuthExpiredError(providerName, text string) error {
	if providerName != "codex" || !looksLikeCodexAuthExpired(text) {
		return nil
	}
	return errors.New("codex account auth is expired; refresh this desktop's account login (auth.json), or update its secret and reload credentials, then start a new session")
}

func looksLikeCodexAuthExpired(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "token_expired") ||
		strings.Contains(lower, "provided authentication token is expired")
}

// formatStartSessionError reformats gRPC errors from StartSession into
// user-friendly multi-line output. Repo setup failures in particular get
// structured with the exit code and indented script output.
func formatStartSessionError(err error) error {
	s, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("start session: %w", err)
	}
	if s.Code() != codes.FailedPrecondition {
		return fmt.Errorf("start session: %s", s.Message())
	}
	msg := strings.TrimPrefix(s.Message(), "start session: ")
	const marker = "repo setup failed: "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return fmt.Errorf("start session: %s", msg)
	}
	detail := msg[idx+len(marker):]

	var b strings.Builder
	b.WriteString("repo setup failed\n\n")

	switch {
	case strings.HasPrefix(detail, "setup timed out"):
		b.WriteString("  " + detail + "\n")
	case strings.HasPrefix(detail, "command failed: "):
		rest := detail[len("command failed: "):]
		if outIdx := strings.Index(rest, "; output: "); outIdx >= 0 {
			fmt.Fprintf(&b, "  %s\n\n", rest[:outIdx])
			output := strings.TrimSpace(rest[outIdx+len("; output: "):])
			b.WriteString("  Output:\n")
			for _, line := range strings.Split(output, "\n") {
				b.WriteString("    " + line + "\n")
			}
		} else {
			b.WriteString("  " + rest + "\n")
		}
	default:
		b.WriteString("  " + detail + "\n")
	}

	return errors.New(strings.TrimRight(b.String(), "\n"))
}

func currentTTYSize() (uint32, uint32) {
	ws, err := pty.GetsizeFull(os.Stdin)
	if err != nil {
		return 120, 40
	}
	return uint32(ws.Cols), uint32(ws.Rows)
}
