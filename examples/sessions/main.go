package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

func main() {
	root := &cobra.Command{
		Use:   "sessions",
		Short: "Manage bridgectl sessions",
	}

	root.AddCommand(newListCmd())
	root.AddCommand(newWatchCmd())
	root.AddCommand(newAttachCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ── connection helpers ────────────────────────────────────────────────────────

// buildClient creates a bridgeclient using either a remote Step CA connection
// or the local server discovered from stateDir.
func buildClient(remote, stateDir string, timeout time.Duration) (*bridgeclient.Client, error) {
	sd := stateDir
	if sd == "" {
		sd = localserver.StateDir()
	}

	if remote != "" {
		return buildRemoteClient(remote, sd, timeout)
	}
	return buildLocalClient(sd, timeout)
}

func buildLocalClient(sd string, timeout time.Duration) (*bridgeclient.Client, error) {
	target, mode := localserver.DiscoverTarget(sd)
	if target == "" {
		return nil, fmt.Errorf("no bridgectl server running (start one with: bridgectl server start)")
	}

	opts := []bridgeclient.Option{
		bridgeclient.WithTarget(target),
		bridgeclient.WithTimeout(timeout),
	}

	if mode == localserver.ModeSecure {
		mat := localserver.LoadPKIMaterial(sd)
		opts = append(opts, bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: mat.CABundlePath,
			CertPath:     mat.LocalClientCert,
			KeyPath:      mat.LocalClientKey,
			ServerName:   "server",
		}))
		opts = append(opts, bridgeclient.WithJWT(bridgeclient.JWTConfig{
			PrivateKeyPath: mat.JWTSigningKey,
			Issuer:         "local",
			Audience:       "bridge",
			TTL:            5 * time.Minute,
		}))
	}

	return bridgeclient.New(opts...)
}

func buildRemoteClient(remote, sd string, timeout time.Duration) (*bridgeclient.Client, error) {
	target := normalizeRemote(remote)
	certsDir := filepath.Join(sd, "certs")

	caBundle := filepath.Join(certsDir, "step-ca-root.crt")
	if _, err := os.Stat(caBundle); err != nil {
		return nil, fmt.Errorf("CA bundle not found at %s — run 'bridgectl client init' to enroll", caBundle)
	}

	certPath, err := discoverClientCert(certsDir)
	if err != nil {
		return nil, fmt.Errorf("no client cert in %s — run 'bridgectl client init' to enroll: %w", certsDir, err)
	}
	keyPath := strings.TrimSuffix(certPath, ".crt") + ".key"
	if _, err := os.Stat(keyPath); err != nil {
		return nil, fmt.Errorf("client key not found: %s", keyPath)
	}

	issuer, err := certCN(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CN from %s: %w", certPath, err)
	}

	// JWT key: prefer certsDir/jwt-signing.key, fallback to sd/jwt-signing.key.
	jwtKeyPath := filepath.Join(certsDir, "jwt-signing.key")
	if _, err := os.Stat(jwtKeyPath); err != nil {
		jwtKeyPath = filepath.Join(sd, "jwt-signing.key")
		if _, err2 := os.Stat(jwtKeyPath); err2 != nil {
			return nil, fmt.Errorf("JWT signing key not found (checked %s/certs/jwt-signing.key and %s/jwt-signing.key) — run 'bridgectl client enroll'", sd, sd)
		}
	}

	return bridgeclient.New(
		bridgeclient.WithTarget(target),
		bridgeclient.WithTimeout(timeout),
		bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: caBundle,
			CertPath:     certPath,
			KeyPath:      keyPath,
			ServerName:   "server",
		}),
		bridgeclient.WithJWT(bridgeclient.JWTConfig{
			PrivateKeyPath: jwtKeyPath,
			Issuer:         issuer,
			Audience:       "bridge",
			TTL:            5 * time.Minute,
		}),
	)
}

// normalizeRemote ensures the address has a port.
func normalizeRemote(remote string) string {
	if strings.Contains(remote, ":") {
		return remote
	}
	return remote + ":9445"
}

// discoverClientCert finds the first non-CA *.crt in certsDir.
func discoverClientCert(certsDir string) (string, error) {
	skip := map[string]bool{
		"step-ca-root.crt": true,
		"ca-bundle.crt":    true,
		"ca.crt":           true,
		"server.crt":       true,
	}
	entries, err := os.ReadDir(certsDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".crt" {
			continue
		}
		if skip[e.Name()] {
			continue
		}
		return filepath.Join(certsDir, e.Name()), nil
	}
	return "", fmt.Errorf("no client certificate found")
}

// certCN reads a PEM certificate and returns the subject CN.
func certCN(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("no PEM block in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return cert.Subject.CommonName, nil
}

// ── list subcommand ───────────────────────────────────────────────────────────

func newListCmd() *cobra.Command {
	var remote, stateDir, project string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := buildClient(remote, stateDir, 15*time.Second)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			resp, err := client.ListSessions(ctx, &bridgev1.ListSessionsRequest{
				ProjectId: project,
			})
			if err != nil {
				return fmt.Errorf("list sessions: %w", err)
			}

			if len(resp.Sessions) == 0 {
				fmt.Println("no sessions")
				return nil
			}

			fmt.Printf("%-38s  %-12s  %-10s  %-10s  %s\n",
				"SESSION ID", "PROJECT", "PROVIDER", "STATUS", "CREATED")
			fmt.Println(strings.Repeat("-", 100))
			for _, s := range resp.Sessions {
				status := strings.TrimPrefix(s.Status.String(), "SESSION_STATUS_")
				created := ""
				if s.CreatedAt != nil {
					created = s.CreatedAt.AsTime().Local().Format("2006-01-02 15:04:05")
				}
				fmt.Printf("%-38s  %-12s  %-10s  %-10s  %s\n",
					s.SessionId, s.ProjectId, s.Provider, status, created)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&remote, "remote", "", "remote hostname or host:port (omit for local server)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "bridge state directory (default: ~/.config/bridgectl)")
	cmd.Flags().StringVar(&project, "project", "", "filter by project ID (empty = all)")
	return cmd
}

// ── watch subcommand ──────────────────────────────────────────────────────────

func newWatchCmd() *cobra.Command {
	var remote, stateDir string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "watch <session-id>",
		Short: "Watch a session (read-only observer)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]

			client, err := buildClient(remote, stateDir, timeout)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
				SessionId: sessionID,
				ClientId:  uuid.NewString(),
				AfterSeq:  0,
				Role:      bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
			})
			if err != nil {
				return fmt.Errorf("attach: %w", err)
			}

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)
			go func() {
				<-sigCh
				cancel()
			}()

			err = stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
				switch ev.Type {
				case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
					_, writeErr := os.Stdout.Write(ev.Payload)
					return writeErr
				case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP:
					_, writeErr := fmt.Fprintf(os.Stderr, "\r\n[bridge] replay gap: oldest=%d last=%d\r\n",
						ev.OldestSeq, ev.LastSeq)
					return writeErr
				case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR:
					return errors.New(ev.Error)
				default:
					return nil
				}
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("stream error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&remote, "remote", "", "remote hostname or host:port (omit for local server)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "bridge state directory (default: ~/.config/bridgectl)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "watch timeout")
	return cmd
}

// ── attach subcommand ─────────────────────────────────────────────────────────

func newAttachCmd() *cobra.Command {
	var remote, stateDir string
	var timeout time.Duration
	var takeOver bool

	cmd := &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Attach to a session interactively (writer)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]

			client, err := buildClient(remote, stateDir, timeout)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			// Check for an existing writer before claiming the slot.
			info, err := client.GetSession(ctx, &bridgev1.GetSessionRequest{
				SessionId: sessionID,
			})
			if err != nil {
				return fmt.Errorf("get session: %w", err)
			}

			if info.ActiveWriterClientId != "" && !takeOver {
				fmt.Fprintf(os.Stderr, "session already has an active writer (client %s)\n", info.ActiveWriterClientId)
				fmt.Fprintln(os.Stderr, "")
				fmt.Fprintln(os.Stderr, "options:")
				fmt.Fprintln(os.Stderr, "  sessions watch <session-id>            — observe without taking the writer slot")
				fmt.Fprintln(os.Stderr, "  sessions attach --take-over <session-id> — forcibly claim the writer slot")
				return fmt.Errorf("writer slot taken")
			}

			clientID := uuid.NewString()

			// Claim the writer slot if taking over.
			if takeOver && info.ActiveWriterClientId != "" {
				cr, claimErr := client.ClaimWriter(ctx, &bridgev1.ClaimWriterRequest{
					SessionId: sessionID,
					ClientId:  clientID,
					Force:     true,
				})
				if claimErr != nil {
					return fmt.Errorf("claim writer: %w", claimErr)
				}
				if !cr.Claimed {
					return fmt.Errorf("writer claim was not granted")
				}
				fmt.Fprintf(os.Stderr, "[bridge] took over from writer %s\n", cr.PreviousWriterClientId)
			}

			stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
				SessionId: sessionID,
				ClientId:  clientID,
				AfterSeq:  0,
				Role:      bridgev1.AttachRole_ATTACH_ROLE_WRITER,
			})
			if err != nil {
				return fmt.Errorf("attach: %w", err)
			}

			fmt.Fprintln(os.Stderr, "[bridge] attached — press Ctrl+\\ to detach")

			restoreTTY, err := setRawTTY()
			if err != nil {
				return fmt.Errorf("configure tty: %w", err)
			}
			var restoreOnce sync.Once
			restore := func() { restoreOnce.Do(restoreTTY) }
			defer restore()

			sigCh := make(chan os.Signal, 2)
			setupSignals(sigCh)
			defer signal.Stop(sigCh)
			go func() {
				for sig := range sigCh {
					switch {
					case isSigwinch(sig):
						cols, rows := currentTTYSize()
						_, _ = client.ResizeSession(context.Background(), &bridgev1.ResizeSessionRequest{
							SessionId: sessionID,
							ClientId:  stream.ClientID(),
							Cols:      cols,
							Rows:      rows,
						})
					case sig == syscall.SIGQUIT:
						// Ctrl+\ — detach cleanly without stopping the session.
						restore()
						fmt.Fprintln(os.Stderr, "\r\n[bridge] detached")
						cancel()
					default:
						// SIGINT / SIGTERM — detach without stopping the session.
						restore()
						cancel()
					}
				}
			}()

			go func() {
				buf := make([]byte, 1024)
				for {
					n, readErr := os.Stdin.Read(buf)
					if n > 0 {
						data := normalizeTTYInput(buf[:n])
						_, _ = client.WriteInput(context.Background(), &bridgev1.WriteInputRequest{
							SessionId: sessionID,
							ClientId:  stream.ClientID(),
							Data:      data,
						})
					}
					if readErr != nil {
						if readErr != io.EOF {
							fmt.Fprintf(os.Stderr, "\r\nstdin read failed: %v\r\n", readErr)
						}
						return
					}
				}
			}()

			err = stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
				switch ev.Type {
				case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
					_, writeErr := os.Stdout.Write(ev.Payload)
					return writeErr
				case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP:
					_, writeErr := fmt.Fprintf(os.Stderr, "\r\n[bridge] replay gap: oldest=%d last=%d\r\n",
						ev.OldestSeq, ev.LastSeq)
					return writeErr
				case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR:
					return errors.New(ev.Error)
				default:
					return nil
				}
			})
			restore()
			if err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("stream error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&remote, "remote", "", "remote hostname or host:port (omit for local server)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "bridge state directory (default: ~/.config/bridgectl)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "attach timeout")
	cmd.Flags().BoolVar(&takeOver, "take-over", false, "forcibly claim the writer slot from an existing writer")
	return cmd
}

// ── TTY helpers ───────────────────────────────────────────────────────────────

func setRawTTY() (func(), error) {
	if _, err := os.Stat("/dev/tty"); err != nil {
		return func() {}, err
	}
	out, err := exec.Command("stty", "-F", "/dev/tty", "-g").Output()
	if err != nil {
		return func() {}, err
	}
	state := string(bytesTrimSpace(out))
	if err := exec.Command("stty", "-F", "/dev/tty", "-icanon", "-echo", "min", "1", "time", "0").Run(); err != nil {
		return func() {}, err
	}
	return func() {
		_ = exec.Command("stty", "-F", "/dev/tty", state).Run()
	}, nil
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == '\n' || b[0] == '\r' || b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

func normalizeTTYInput(b []byte) []byte {
	data := append([]byte(nil), b...)
	for i := range data {
		if data[i] == '\n' {
			data[i] = '\r'
		}
	}
	return data
}

func currentTTYSize() (uint32, uint32) {
	ws, err := pty.GetsizeFull(os.Stdin)
	if err != nil {
		return 120, 40
	}
	return uint32(ws.Cols), uint32(ws.Rows)
}
