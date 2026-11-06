package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"golang.org/x/term"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
)

// runRemoteSession starts a new session on a remote bridge server and attaches
// an interactive terminal. It uses mTLS + JWT credentials (auto-discovered or
// explicitly provided) to connect, creates a session via the StartSession RPC,
// and then attaches with AttachSession + WriteInput.
func runRemoteSession(dir, providerName, project string, timeout time.Duration, remote, certOverride, keyOverride, jwtKeyOverride, serverNameOverride string) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}

	client, err := connectRemoteClient(remote, timeout, certOverride, keyOverride, jwtKeyOverride, serverNameOverride)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	client.SetProject(project)

	ws, wsErr := pty.GetsizeFull(os.Stdin)
	var cols, rows uint32
	if wsErr != nil {
		cols, rows = 120, 40
	} else {
		cols, rows = uint32(ws.Cols), uint32(ws.Rows)
	}

	sessionID := uuid.NewString()
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
	display := newAttachDisplay(os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdout.Fd())))
	err = stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
		switch ev.Type {
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
			if err := codexOutputAuthExpiredError(providerName, &outputTail, ev.Payload); err != nil {
				return err
			}
			_, writeErr := os.Stdout.Write(ev.Payload)
			return writeErr
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP:
			return display.replayGap(ev)
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
	display.printDeferredNotices()

	if detached.Load() {
		fmt.Fprintf(os.Stderr, "\r\nDetached from session %s\r\n", sessionID)
		fmt.Fprintf(os.Stderr, "Reattach with: bridgectl session attach --remote %s %s\r\n", remote, sessionID)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "\r\nsession ended: %v\r\n", err)
	}
	return nil
}

// runRemoteSessionNoTTY runs a session on a remote bridge server without a
// terminal, forwarding raw stdin to the provider and writing output to stdout.
func runRemoteSessionNoTTY(dir, providerName, project string, timeout time.Duration, remote, certOverride, keyOverride, jwtKeyOverride, serverNameOverride string) error {
	client, err := connectRemoteClient(remote, timeout, certOverride, keyOverride, jwtKeyOverride, serverNameOverride)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	client.SetProject(project)

	sessionID := uuid.NewString()
	clientID := uuid.NewString()
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
