package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

func main() {
	remote := flag.String("remote", "", "remote hostname or host:port (required; port defaults to 9445)")
	stateDir := flag.String("state-dir", "", "state dir for credential discovery (default: ~/.config/bridgectl)")
	certFlag := flag.String("cert", "", "override client cert path")
	keyFlag := flag.String("key", "", "override client key path (derived from --cert if omitted)")
	jwtKeyFlag := flag.String("jwt-key", "", "override JWT signing key path")
	project := flag.String("project", "local", "project ID")
	provider := flag.String("provider", "claude", "provider name")
	timeout := flag.Duration("timeout", 30*time.Minute, "session timeout")
	flag.Parse()

	if *remote == "" {
		fmt.Fprintln(os.Stderr, "error: --remote is required")
		fmt.Fprintln(os.Stderr, "usage: chat-ca --remote <host>[:<port>] [flags] <repo-path>")
		os.Exit(1)
	}
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: chat-ca --remote <host>[:<port>] [flags] <repo-path>")
		os.Exit(1)
	}
	repoPath := flag.Arg(0)

	sd := *stateDir
	if sd == "" {
		sd = localserver.StateDir()
	}

	target := normalizeRemote(*remote)

	certPath, keyPath, jwtKeyPath, issuer, err := resolveRemoteCreds(sd, *certFlag, *keyFlag, *jwtKeyFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "To set up remote credentials:")
		fmt.Fprintln(os.Stderr, "  bridgectl client init    — enroll with the remote Step CA")
		fmt.Fprintln(os.Stderr, "  bridgectl client enroll  — re-enroll an existing identity")
		os.Exit(1)
	}

	certsDir := filepath.Join(sd, "certs")
	caBundle := filepath.Join(certsDir, "step-ca-root.crt")
	if _, statErr := os.Stat(caBundle); statErr != nil {
		fmt.Fprintf(os.Stderr, "error: CA bundle not found: %s\n", caBundle)
		fmt.Fprintln(os.Stderr, "Run 'bridgectl client init' to enroll with the remote Step CA")
		os.Exit(1)
	}

	client, err := bridgeclient.New(
		bridgeclient.WithTarget(target),
		bridgeclient.WithTimeout(*timeout),
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
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = client.Close() }()
	client.SetProject(*project)

	cols, rows := currentTTYSize()
	sessionID := uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if _, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   *project,
		SessionId:   sessionID,
		RepoPath:    repoPath,
		Provider:    *provider,
		InitialCols: cols,
		InitialRows: rows,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start session: %v\n", err)
		os.Exit(1)
	}

	restoreTTY, err := setRawTTY()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure local tty: %v\n", err)
		os.Exit(1)
	}
	var restoreOnce sync.Once
	restore := func() {
		restoreOnce.Do(restoreTTY)
	}
	defer restore()

	stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  uuid.NewString(),
		AfterSeq:  0,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to attach session: %v\n", err)
		os.Exit(1)
	}

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
			default:
				cancel()
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, _ = client.StopSession(stopCtx, &bridgev1.StopSessionRequest{
					SessionId: sessionID,
					Force:     true,
				})
				stopCancel()
				restore()
				os.Exit(0)
			}
		}
	}()

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				data := normalizeTTYInput(buf[:n])
				_, _ = client.WriteInput(context.Background(), &bridgev1.WriteInputRequest{
					SessionId: sessionID,
					ClientId:  stream.ClientID(),
					Data:      data,
				})
			}
			if err != nil {
				if err != io.EOF {
					fmt.Fprintf(os.Stderr, "\r\nstdin read failed: %v\r\n", err)
				}
				return
			}
		}
	}()

	err = stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
		switch ev.Type {
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
			_, err := os.Stdout.Write(ev.Payload)
			return err
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP:
			_, err := fmt.Fprintf(os.Stderr, "\r\n[bridge] replay gap: oldest=%d last=%d\r\n", ev.OldestSeq, ev.LastSeq)
			return err
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR:
			return errors.New(ev.Error)
		default:
			return nil
		}
	})
	restore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\r\nstream failed: %v\r\n", err)
		os.Exit(1)
	}
}

// normalizeRemote ensures the remote address includes a port.
func normalizeRemote(remote string) string {
	if strings.Contains(remote, ":") {
		return remote
	}
	return remote + ":9445"
}

// resolveRemoteCreds finds the client cert, key, JWT key, and issuer for a
// remote Step CA connection. Explicit overrides take precedence; otherwise
// credentials are auto-discovered from stateDir/certs/.
func resolveRemoteCreds(stateDir, certOverride, keyOverride, jwtKeyOverride string) (certPath, keyPath, jwtKeyPath, issuer string, err error) {
	certsDir := filepath.Join(stateDir, "certs")

	// Resolve client cert.
	if certOverride != "" {
		certPath = certOverride
	} else {
		certPath, err = discoverClientCert(certsDir)
		if err != nil {
			return "", "", "", "", fmt.Errorf("could not find client cert in %s: %w", certsDir, err)
		}
	}

	// Resolve client key.
	if keyOverride != "" {
		keyPath = keyOverride
	} else {
		keyPath = strings.TrimSuffix(certPath, ".crt") + ".key"
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		return "", "", "", "", fmt.Errorf("client key not found: %s", keyPath)
	}

	// Derive issuer from the CN of the client cert.
	issuer, err = certCN(certPath)
	if err != nil {
		return "", "", "", "", fmt.Errorf("read client cert CN from %s: %w", certPath, err)
	}

	// Resolve JWT signing key.
	if jwtKeyOverride != "" {
		jwtKeyPath = jwtKeyOverride
	} else {
		// Prefer certsDir/jwt-signing.key; fall back to stateDir/jwt-signing.key.
		primary := filepath.Join(certsDir, "jwt-signing.key")
		fallback := filepath.Join(stateDir, "jwt-signing.key")
		if _, statErr := os.Stat(primary); statErr == nil {
			jwtKeyPath = primary
		} else if _, statErr := os.Stat(fallback); statErr == nil {
			jwtKeyPath = fallback
		} else {
			return "", "", "", "", fmt.Errorf("JWT signing key not found (checked %s and %s)", primary, fallback)
		}
	}

	return certPath, keyPath, jwtKeyPath, issuer, nil
}

// discoverClientCert finds the first non-CA *.crt file in certsDir.
// It skips step-ca-root.crt, ca-bundle.crt, ca.crt, and server.crt.
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

// certCN reads a PEM-encoded certificate file and returns the subject CN.
func certCN(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return cert.Subject.CommonName, nil
}

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
