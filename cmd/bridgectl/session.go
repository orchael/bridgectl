package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage agent sessions",
	}

	cmd.AddCommand(
		newSessionStartCmd(),
		newSessionListCmd(),
		newSessionAttachCmd(),
		newSessionWatchCmd(),
		newSessionStopCmd(),
	)

	return cmd
}

// These are the standard flags for connecting to a remote bridge server.
func addRemoteFlags(cmd *cobra.Command, remote, cert, key, jwtKey, serverName *string) {
	cmd.Flags().StringVar(remote, "remote", "", "remote bridge server hostname (e.g. macbook.ts.net or macbook.ts.net:9445)")
	cmd.Flags().StringVar(cert, "cert", "", "path to client certificate (auto-discovered from ~/.config/bridgectl/certs/ if omitted)")
	cmd.Flags().StringVar(key, "key", "", "path to client private key (derived from --cert if omitted)")
	cmd.Flags().StringVar(jwtKey, "jwt-key", "", "path to JWT signing key (auto-discovered from ~/.config/bridgectl/certs/ if omitted)")
	cmd.Flags().StringVar(serverName, "server-name", "", "TLS server name to verify (defaults to host from --remote)")
}

func newSessionListCmd() *cobra.Command {
	var (
		project    string
		remote     string
		cert       string
		key        string
		jwtKey     string
		serverName string
	)

	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List active sessions",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := connectClientForHost(remote, 5*time.Second, cert, key, jwtKey, serverName)
			if err != nil {
				if remote != "" {
					return err
				}
				fmt.Println("No bridgectl server running.")
				return nil
			}
			defer func() { _ = client.Close() }()
			client.SetProject(project)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			resp, err := client.ListSessions(ctx, &bridgev1.ListSessionsRequest{
				ProjectId: project,
			})
			if err != nil {
				if hint := remoteAuthHint(err, remote, jwtKey); hint != "" {
					return fmt.Errorf("list sessions: %w\n\n%s", err, hint)
				}
				return fmt.Errorf("list sessions: %w", err)
			}

			if len(resp.Sessions) == 0 {
				fmt.Println("No active sessions.")
				return nil
			}

			sort.Slice(resp.Sessions, func(i, j int) bool {
				return resp.Sessions[i].CreatedAt.AsTime().Before(resp.Sessions[j].CreatedAt.AsTime())
			})

			fmt.Printf("%-36s  %-10s  %-10s  %s\n", "SESSION ID", "PROVIDER", "STATUS", "CREATED")
			for _, s := range resp.Sessions {
				status := sessionStatusString(s.Status)
				created := s.CreatedAt.AsTime().Format("15:04:05")
				fmt.Printf("%-36s  %-10s  %-10s  %s\n", s.SessionId, s.Provider, status, created)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&project, "project", "local", "project ID to filter")
	addRemoteFlags(cmd, &remote, &cert, &key, &jwtKey, &serverName)
	return cmd
}

func remoteAuthHint(err error, remote, jwtKey string) string {
	if remote == "" || jwtKey != "" || !errors.Is(err, bridgeclient.ErrUnauthorized) {
		return ""
	}

	localJWTKey, statErr := filepath.Abs("jwt-signing.key")
	if statErr != nil {
		localJWTKey = "jwt-signing.key"
	}
	if _, statErr := os.Stat(localJWTKey); statErr != nil {
		return ""
	}

	return fmt.Sprintf(
		"Found a JWT signing key in the current directory. Remote commands use JWT keys from ~/.config/bridgectl/certs/ or ~/.config/bridgectl unless --jwt-key is set. If this local key is intended, retry with:\n  bridgectl session list --remote %s --jwt-key %s",
		remote,
		localJWTKey,
	)
}

func newSessionAttachCmd() *cobra.Command {
	var (
		observeOnly bool
		takeOver    bool
		release     bool
		remote      string
		cert        string
		key         string
		jwtKey      string
		serverName  string
	)

	cmd := &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Attach to a running session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			if release {
				return releaseWriter(sessionID, remote, cert, key, jwtKey, serverName)
			}
			role := bridgev1.AttachRole_ATTACH_ROLE_WRITER
			if observeOnly {
				role = bridgev1.AttachRole_ATTACH_ROLE_OBSERVER
			}
			return attachSession(sessionID, role, takeOver, remote, cert, key, jwtKey, serverName)
		},
	}

	cmd.Flags().BoolVar(&observeOnly, "observe", false, "attach as read-only observer (no input)")
	cmd.Flags().BoolVar(&takeOver, "take-over", false, "forcibly claim the writer slot from the current active writer")
	cmd.Flags().BoolVar(&release, "release", false, "release the current active writer slot (affects whoever currently holds it)")
	addRemoteFlags(cmd, &remote, &cert, &key, &jwtKey, &serverName)
	return cmd
}

func newSessionWatchCmd() *cobra.Command {
	var (
		remote     string
		cert       string
		key        string
		jwtKey     string
		serverName string
	)

	cmd := &cobra.Command{
		Use:   "watch <session-id>",
		Short: "Watch a running session (read-only)",
		Long:  "Attach to a running session as a read-only observer. Shorthand for 'session attach <session-id> --observe'.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return attachSession(args[0], bridgev1.AttachRole_ATTACH_ROLE_OBSERVER, false, remote, cert, key, jwtKey, serverName)
		},
	}

	addRemoteFlags(cmd, &remote, &cert, &key, &jwtKey, &serverName)
	return cmd
}

func newSessionStopCmd() *cobra.Command {
	var (
		force      bool
		remote     string
		cert       string
		key        string
		jwtKey     string
		serverName string
	)

	cmd := &cobra.Command{
		Use:   "stop <session-id>",
		Short: "Stop a running session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]

			client, err := connectClientForHost(remote, 10*time.Second, cert, key, jwtKey, serverName)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err = client.StopSession(ctx, &bridgev1.StopSessionRequest{
				SessionId: sessionID,
				Force:     force,
			})
			if err != nil {
				return fmt.Errorf("stop session: %w", err)
			}
			fmt.Printf("Session %s stopped.\n", sessionID)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "force kill (SIGKILL)")
	addRemoteFlags(cmd, &remote, &cert, &key, &jwtKey, &serverName)
	return cmd
}

func attachSession(sessionID string, role bridgev1.AttachRole, takeOver bool, remote, cert, key, jwtKey, serverName string) error {
	client, err := connectClientForHost(remote, 30*time.Minute, cert, key, jwtKey, serverName)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	fd := int(os.Stdin.Fd())
	isObserver := role == bridgev1.AttachRole_ATTACH_ROLE_OBSERVER && !takeOver
	var writerReady atomic.Bool
	var restore func()
	if term.IsTerminal(fd) {
		// When stdin is a TTY, enable raw mode for both writers and observers.
		// Writers need it for interactive input. Observers need it so that
		// the terminal's output post-processing (OPOST/ONLCR) does not
		// corrupt ANSI escape sequences from the session PTY — without raw
		// mode, every \n in the PTY stream gets an extra \r prepended,
		// garbling TUI output. Non-TTY observers skip raw mode entirely.
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("set raw terminal: %w", err)
		}
		var restoreOnce sync.Once
		restore = func() {
			restoreOnce.Do(func() {
				if !isObserver {
					// Discard unread keystrokes on every writer exit, including
					// when the stream ends before its input goroutine starts.
					if err := discardTerminalInput(fd); err != nil {
						fmt.Fprintf(os.Stderr, "discard terminal input: %v\r\n", err)
					}
				}
				_ = term.Restore(fd, oldState)
			})
		}
		defer restore()
	} else if !isObserver {
		return fmt.Errorf("stdin is not a terminal")
	} else {
		restore = func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientID := uuid.NewString()

	// When --take-over is set, attach as observer first, then claim the writer
	// slot with force. This way the ClaimWriter RPC evicts the existing writer
	// before we start sending input.
	attachRole := role
	if takeOver {
		attachRole = bridgev1.AttachRole_ATTACH_ROLE_OBSERVER
	}

	stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  clientID,
		AfterSeq:  0,
		Role:      attachRole,
	})
	if err != nil {
		restore()
		return fmt.Errorf("attach: %w", err)
	}

	isWriter := role == bridgev1.AttachRole_ATTACH_ROLE_WRITER || takeOver
	var detached atomic.Bool
	var attachmentReady bool
	var claimErr error
	var sessionExit string

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	setupSigwinch(sigCh)
	defer signal.Stop(sigCh)
	var resizeMu sync.Mutex
	resize := func() {
		resizeMu.Lock()
		defer resizeMu.Unlock()
		c, r := currentTTYSize()
		_, _ = client.ResizeSession(ctx, &bridgev1.ResizeSessionRequest{
			SessionId: sessionID,
			ClientId:  stream.ClientID(),
			Cols:      c,
			Rows:      r,
		})
	}

	go func() {
		handleAttachSignals(ctx, sigCh, isWriter, func() {
			if !writerReady.Load() {
				return
			}
			resize()
		}, cancel)
	}()

	startWriter := func() {
		go func() {
			w := &inputWriter{cfg: inputWriterConfig{
				Reader:    os.Stdin,
				Client:    client,
				SessionID: sessionID,
				ClientID:  stream.ClientID(),
				DetachKey: detachKey,
				OnDetach: func() {
					// Release the writer slot before detaching so another
					// observer can claim it.
					_, _ = client.ReleaseWriter(context.Background(), &bridgev1.ReleaseWriterRequest{
						SessionId: sessionID,
						ClientId:  stream.ClientID(),
					})
					detached.Store(true)
					cancel()
				},
			}}
			w.Run()
		}()
	}
	if !isWriter && term.IsTerminal(fd) {
		// In raw mode ISIG is disabled, so the terminal won't generate
		// SIGINT for Ctrl+C. Read stdin and handle it manually.
		go func() {
			buf := make([]byte, 256)
			for {
				n, readErr := os.Stdin.Read(buf)
				for i := 0; i < n; i++ {
					if buf[i] == 0x03 || buf[i] == detachKey { // Ctrl+C or Ctrl+]
						detached.Store(true)
						cancel()
						return
					}
				}
				if readErr != nil {
					return
				}
			}
		}()
	}

	err = stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
		switch ev.Type {
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED:
			attachmentReady = true
			// AttachSession only prepares the SDK wrapper; RecvAll opens the
			// stream. The server must confirm attachment before we claim or write.
			if isWriter && !writerReady.Load() {
				if takeOver {
					_, claimErr = client.ClaimWriter(ctx, &bridgev1.ClaimWriterRequest{
						SessionId: sessionID,
						ClientId:  clientID,
						Force:     true,
					})
					if claimErr != nil {
						claimErr = fmt.Errorf("claim writer: %w", claimErr)
						return claimErr
					}
				}
				writerReady.Store(true)
				resize()
				startWriter()
			}
			return nil
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
			_, writeErr := os.Stdout.Write(ev.Payload)
			return writeErr
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP:
			_, writeErr := fmt.Fprintf(os.Stderr, "\r\n[bridgectl] replay gap: oldest=%d last=%d\r\n", ev.OldestSeq, ev.LastSeq)
			return writeErr
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR:
			return errors.New(ev.Error)
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_SESSION_EXIT:
			sessionExit = sessionExitMessage(ev)
			return errSessionExit
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_CLAIMED:
			_, writeErr := fmt.Fprintf(os.Stderr, "\r\n[bridgectl] writer claimed by %s\r\n", ev.WriterClientId)
			return writeErr
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED:
			_, writeErr := fmt.Fprintf(os.Stderr, "\r\n[bridgectl] writer released by %s\r\n", ev.WriterClientId)
			return writeErr
		default:
			return nil
		}
	})
	restore()
	if claimErr != nil {
		return claimErr
	}

	if detached.Load() {
		fmt.Fprintf(os.Stderr, "\r\nDetached from session %s\r\n", sessionID)
		fmt.Fprintf(os.Stderr, "Reattach with: bridgectl session attach %s\r\n", sessionID)
		return nil
	}
	if sessionExit != "" {
		fmt.Fprintf(os.Stderr, "\r\n%s\r\n", sessionExit)
		return nil
	}
	if !attachmentReady && ctx.Err() == nil {
		if err == nil {
			return fmt.Errorf("attach: stream ended before attachment was acknowledged")
		}
		return fmt.Errorf("attach: %w", err)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\r\nsession ended: %v\r\n", err)
	}
	return nil
}

// releaseWriter sends a ReleaseWriter RPC for sessionID targeting whoever
// currently holds the active writer slot (not necessarily this client).
// This is a fire-and-forget command: it doesn't attach a stream.
func releaseWriter(sessionID, remote, cert, key, jwtKey, serverName string) error {
	client, err := connectClientForHost(remote, 10*time.Second, cert, key, jwtKey, serverName)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// We need a stable client-id for this operation; obtain it from the session info.
	resp, err := client.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: sessionID})
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	writerID := resp.ActiveWriterClientId
	if writerID == "" {
		fmt.Fprintf(os.Stderr, "Session %s has no active writer.\n", sessionID)
		return nil
	}
	_, err = client.ReleaseWriter(ctx, &bridgev1.ReleaseWriterRequest{
		SessionId: sessionID,
		ClientId:  writerID,
	})
	if err != nil {
		return fmt.Errorf("release writer: %w", err)
	}
	fmt.Printf("Released writer slot for session %s (was: %s)\n", sessionID, writerID)
	return nil
}

func sessionStatusString(s bridgev1.SessionStatus) string {
	switch s {
	case bridgev1.SessionStatus_SESSION_STATUS_STARTING:
		return "starting"
	case bridgev1.SessionStatus_SESSION_STATUS_RUNNING:
		return "running"
	case bridgev1.SessionStatus_SESSION_STATUS_ATTACHED:
		return "attached"
	case bridgev1.SessionStatus_SESSION_STATUS_STOPPING:
		return "stopping"
	case bridgev1.SessionStatus_SESSION_STATUS_STOPPED:
		return "stopped"
	case bridgev1.SessionStatus_SESSION_STATUS_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}
