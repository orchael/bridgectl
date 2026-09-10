package cli_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/orchael/bridgectl/internal/pki"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
	"github.com/stretchr/testify/suite"
)

// cliBinary holds the path to the compiled bridgectl binary.
// It is built once per test run via TestMain.
var cliBinary string

// CLISuite groups all CLI e2e tests with shared setup/teardown.
type CLISuite struct {
	suite.Suite
}

// TestCLISuite is the entry point that runs all CLI e2e tests.
func TestCLISuite(t *testing.T) {
	suite.Run(t, new(CLISuite))
}

func TestMain(m *testing.M) {
	// Build the bridgectl binary into a temp dir.
	dir, err := os.MkdirTemp("", "cli-e2e-*")
	if err != nil {
		panic(err)
	}

	bin := filepath.Join(dir, "bridgectl")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	buildArgs := []string{"build", "-o", bin}
	if coverDir := os.Getenv("BRIDGECTL_CLI_COVERAGE_DIR"); coverDir != "" {
		// Subprocess coverage must be collected separately from the test
		// binary's profile; the coverage script merges these CLI counters.
		buildArgs = append(buildArgs, "-cover", "-covermode=atomic", "-coverpkg=github.com/orchael/bridgectl/cmd/bridgectl")
		if err := os.Setenv("GOCOVERDIR", coverDir); err != nil {
			panic(err)
		}
	}
	buildArgs = append(buildArgs, "../../cmd/bridgectl")
	cmd := exec.Command("go", buildArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("failed to build bridgectl binary: " + err.Error())
	}
	cliBinary = bin

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testStateDir returns a per-test temp state dir (isolated from ~/.config/bridgectl).
func (s *CLISuite) testStateDir() string {
	s.T().Helper()
	dir := s.T().TempDir()
	s.T().Setenv("BRIDGECTL_STATE_DIR", dir)
	return dir
}

// TestServerStartStop verifies that the server starts and stops cleanly.
func (s *CLISuite) TestServerStartStop() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// Start server via the localserver package directly.
	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err, "server should start")
	defer srv.Stop()

	// Verify server is discoverable.
	target, _ := localserver.DiscoverTarget(stateDir)
	s.Require().NotEmpty(target, "should discover running server")

	// Health check via SDK.
	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Health(ctx)
	s.Require().NoError(err)
	s.Assert().NotEmpty(resp.ServerInstanceId)

	// Stop server.
	srv.Stop()

	// Verify server is no longer discoverable.
	s.Assert().False(localserver.IsServerRunning(stateDir))
}

// TestEchoSessionLifecycle tests creating, listing, and stopping a session
// using the echo (cat) provider.
func (s *CLISuite) TestEchoSessionLifecycle() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Target()
	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start a session.
	sessionID := uuid.NewString()
	startResp, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "test",
		SessionId:   sessionID,
		RepoPath:    repoDir,
		Provider:    "echo",
		InitialCols: 80,
		InitialRows: 24,
	})
	s.Require().NoError(err)
	s.Assert().Equal(sessionID, startResp.SessionId)

	// Wait for the session to be running.
	var info *bridgev1.GetSessionResponse
	for i := 0; i < 20; i++ {
		info, err = client.GetSession(ctx, &bridgev1.GetSessionRequest{
			SessionId: sessionID,
		})
		if err == nil && info.Status != bridgev1.SessionStatus_SESSION_STATUS_STARTING {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Require().NoError(err)
	s.Assert().Equal(sessionID, info.SessionId)

	// List sessions.
	listResp, err := client.ListSessions(ctx, &bridgev1.ListSessionsRequest{
		ProjectId: "test",
	})
	s.Require().NoError(err)
	s.Assert().GreaterOrEqual(len(listResp.Sessions), 1)
	found := false
	for _, s := range listResp.Sessions {
		if s.SessionId == sessionID {
			found = true
		}
	}
	s.Assert().True(found, "session should appear in list")

	// WriteInput may fail if no client is attached — that's OK for this test.
	// The important thing is the session is running.
	_, _ = client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  "test-client",
		Data:      []byte("hello\n"),
	})

	// Stop session.
	_, err = client.StopSession(ctx, &bridgev1.StopSessionRequest{
		SessionId: sessionID,
		Force:     true,
	})
	s.Require().NoError(err)

	// Verify session is stopped.
	time.Sleep(200 * time.Millisecond)
	info, err = client.GetSession(ctx, &bridgev1.GetSessionRequest{
		SessionId: sessionID,
	})
	s.Require().NoError(err)
	s.Assert().True(
		info.Status == bridgev1.SessionStatus_SESSION_STATUS_STOPPED ||
			info.Status == bridgev1.SessionStatus_SESSION_STATUS_FAILED,
		"session should be stopped or failed, got %v", info.Status)
}

func (s *CLISuite) TestRepoSetupConfigEnvironmentPropagation() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()
	binDir := s.T().TempDir()
	providerScript := filepath.Join(binDir, "print-setup-env.sh")
	s.Require().NoError(os.WriteFile(providerScript, []byte(`#!/bin/sh
printf 'SETUP_VALUE=%s\n' "$BRIDGE_E2E_SETUP_VALUE"
sleep 5
`), 0o755))
	configPath := filepath.Join(s.T().TempDir(), "bridge.yaml")
	s.Require().NoError(os.WriteFile(configPath, []byte(`
providers:
  setupenv:
    binary: `+strconv.Quote(providerScript)+`
    startup_probe: output
    startup_timeout: 5s
    required_env: ["BRIDGE_E2E_SETUP_VALUE"]
repo_setup:
  default_timeout: 5s
  max_timeout: 30s
`), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(repoDir, ".bridgectl.yaml"), []byte(`
version: 1
shell: bash
setup:
  - export BRIDGE_E2E_SETUP_VALUE=from-repo-setup
`), 0o644))

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ConfigPath: configPath,
	})
	s.Require().NoError(err)
	defer srv.Stop()

	client, err := bridgeclient.New(bridgeclient.WithTarget(srv.Target()))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sessionID := uuid.NewString()
	_, err = client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "test",
		SessionId:   sessionID,
		RepoPath:    repoDir,
		Provider:    "setupenv",
		InitialCols: 80,
		InitialRows: 24,
	})
	s.Require().NoError(err)

	_, attached, getEvents, stopRecv := s.attachAndCollectEvents(ctx, client, sessionID, uuid.NewString(), bridgev1.AttachRole_ATTACH_ROLE_WRITER)
	defer stopRecv()
	s.waitForAttach(attached)

	s.Require().Eventually(func() bool {
		for _, ev := range getEvents() {
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT && strings.Contains(string(ev.Payload), "SETUP_VALUE=from-repo-setup") {
				return true
			}
		}
		return false
	}, 5*time.Second, 100*time.Millisecond)

	_, err = client.StopSession(ctx, &bridgev1.StopSessionRequest{SessionId: sessionID, Force: true})
	s.Require().NoError(err)
}

// TestAutoServerDiscovery tests that a second client discovers the first server.
func (s *CLISuite) TestAutoServerDiscovery() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// Start first server.
	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)
	defer srv.Stop()

	// The second "instance" should discover the existing server.
	target, _ := localserver.DiscoverTarget(stateDir)
	s.Require().NotEmpty(target, "second instance should discover existing server")
	s.Assert().Equal(srv.Target(), target, "should discover the same server")

	// Verify health from second connection.
	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Health(ctx)
	s.Require().NoError(err)
	s.Assert().NotEmpty(resp.ServerInstanceId)
}

// TestMultipleSessionsSameServer tests that multiple sessions can run on the
// same server (simulating multiple terminal windows).
func (s *CLISuite) TestMultipleSessionsSameServer() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Target()
	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start two sessions.
	sessionA := uuid.NewString()
	sessionB := uuid.NewString()
	for _, id := range []string{sessionA, sessionB} {
		_, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{
			ProjectId:   "test",
			SessionId:   id,
			RepoPath:    repoDir,
			Provider:    "echo",
			InitialCols: 80,
			InitialRows: 24,
		})
		s.Require().NoError(err, "should start session %s", id)
	}

	// Wait briefly for sessions to start.
	time.Sleep(500 * time.Millisecond)

	// List sessions — both should be present.
	listResp, err := client.ListSessions(ctx, &bridgev1.ListSessionsRequest{
		ProjectId: "test",
	})
	s.Require().NoError(err)
	ids := make(map[string]bool)
	for _, s := range listResp.Sessions {
		ids[s.SessionId] = true
	}
	s.Assert().True(ids[sessionA], "session-a should be listed")
	s.Assert().True(ids[sessionB], "session-b should be listed")

	// Stop both.
	for _, id := range []string{sessionA, sessionB} {
		_, err := client.StopSession(ctx, &bridgev1.StopSessionRequest{
			SessionId: id,
			Force:     true,
		})
		s.Require().NoError(err)
	}
}

// TestServerDoesNotDoubleStart verifies that discovery finds the already-running
// server, so a second caller would connect to it instead of starting a new one.
func (s *CLISuite) TestServerDoesNotDoubleStart() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	srv1, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)
	defer srv1.Stop()

	// Detect that server is running.
	s.Assert().True(localserver.IsServerRunning(stateDir))

	// Discovery should find the existing server.
	target, _ := localserver.DiscoverTarget(stateDir)
	s.Require().NotEmpty(target)
	s.Assert().Equal(srv1.Target(), target)
}

// TestCLIVersion tests that `bridgectl --version` works.
func (s *CLISuite) TestCLIVersion() {
	cmd := exec.Command(cliBinary, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	s.Require().NoError(err, "--version should succeed")
	s.Assert().Contains(out.String(), "bridgectl version")
}

// TestCLIHelp tests that `bridgectl --help` exits cleanly.
func (s *CLISuite) TestCLIHelp() {
	cmd := exec.Command(cliBinary, "--help")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	s.Require().NoError(err, "--help should succeed")
	s.Assert().Contains(out.String(), "bridgectl starts a local bridge server")
}

// TestCLISessionListNoServer tests that `session list` handles no server gracefully.
func (s *CLISuite) TestCLISessionListNoServer() {
	stateDir := s.testStateDir()

	cmd := exec.Command(cliBinary, "session", "list")
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	// Should succeed but print "No bridgectl server running."
	s.Require().NoError(err)
	s.Assert().Contains(out.String(), "No bridgectl server running")
}

// TestCLIServerStatus tests `server status` output.
func (s *CLISuite) TestCLIServerStatus() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// No server running.
	cmd := exec.Command(cliBinary, "server", "status")
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	s.Require().NoError(err)
	s.Assert().Contains(out.String(), "not running")

	// Start server.
	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)
	defer srv.Stop()

	// Now status should show running.
	cmd = exec.Command(cliBinary, "server", "status")
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	out.Reset()
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	s.Require().NoError(err)
	output := out.String()
	s.Assert().Contains(output, "running")
}

// TestProviderEchoAvailable verifies the echo provider shows as available.
func (s *CLISuite) TestProviderEchoAvailable() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)
	defer srv.Stop()

	client, err := bridgeclient.New(bridgeclient.WithTarget(srv.Target()))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Health(ctx)
	s.Require().NoError(err)

	found := false
	for _, p := range resp.Providers {
		if p.Provider == "echo" {
			found = true
			s.Assert().True(p.Available, "echo provider should be available")
		}
	}
	s.Assert().True(found, "echo provider should be listed in health response")
}

// TestCleanShutdownCleansFiles verifies state files are removed on stop.
func (s *CLISuite) TestCleanShutdownCleansFiles() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)

	// Files should exist.
	_, err = os.Stat(filepath.Join(stateDir, "server.pid"))
	s.Assert().NoError(err, "PID file should exist")
	_, err = os.Stat(filepath.Join(stateDir, "server.addr"))
	s.Assert().NoError(err, "addr file should exist")

	if runtime.GOOS != "windows" {
		_, err = os.Stat(filepath.Join(stateDir, "server.sock"))
		s.Assert().NoError(err, "socket should exist")
	}

	srv.Stop()

	// Files should be cleaned up.
	_, err = os.Stat(filepath.Join(stateDir, "server.pid"))
	s.Assert().True(os.IsNotExist(err), "PID file should be removed")
	_, err = os.Stat(filepath.Join(stateDir, "server.addr"))
	s.Assert().True(os.IsNotExist(err), "addr file should be removed")
}

// TestStaleSocketRecovery verifies that a stale unix socket is cleaned up.
func (s *CLISuite) TestStaleSocketRecovery() {
	if runtime.GOOS == "windows" {
		s.T().Skip("unix sockets not used on Windows")
	}
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// Create a stale socket file.
	sockPath := filepath.Join(stateDir, "server.sock")
	if err := os.WriteFile(sockPath, []byte("stale"), 0o644); err != nil {
		s.T().Fatal(err)
	}

	// IsServerRunning should return false (stale socket, no listener).
	s.Assert().False(localserver.IsServerRunning(stateDir))

	// Start should succeed by replacing the stale socket.
	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)
	defer srv.Stop()

	s.Assert().True(localserver.IsServerRunning(stateDir))
}

// TestSessionAttachAndInput tests attaching to a session and writing input.
func (s *CLISuite) TestSessionAttachAndInput() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir: stateDir,
	})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Target()
	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sessionID := uuid.NewString()
	_, err = client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "test",
		SessionId:   sessionID,
		RepoPath:    repoDir,
		Provider:    "echo",
		InitialCols: 80,
		InitialRows: 24,
	})
	s.Require().NoError(err)

	// Wait for session to be running.
	time.Sleep(500 * time.Millisecond)

	// Attach and read output in a goroutine (RecvAll opens the gRPC stream).
	clientID := uuid.NewString()
	stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  clientID,
		AfterSeq:  0,
	})
	s.Require().NoError(err)

	var received strings.Builder
	var mu sync.Mutex
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()

	attached := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = stream.RecvAll(readCtx, func(ev *bridgev1.AttachSessionEvent) error {
			switch ev.Type {
			case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED:
				select {
				case attached <- struct{}{}:
				default:
				}
			case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
				mu.Lock()
				received.Write(ev.Payload)
				got := received.String()
				mu.Unlock()
				if strings.Contains(got, "HELLO_FROM_E2E") {
					readCancel()
				}
			}
			return nil
		})
	}()

	// Wait for the attach event before writing input.
	select {
	case <-attached:
	case <-time.After(3 * time.Second):
		s.T().Log("timeout waiting for attach event, trying write anyway")
	}

	// Write some input. The echo provider (cat) echoes it back.
	testMsg := "HELLO_FROM_E2E\n"
	_, err = client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  stream.ClientID(),
		Data:      []byte(testMsg),
	})
	s.Require().NoError(err)

	// Wait for output — require it to arrive.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}

	mu.Lock()
	got := received.String()
	mu.Unlock()
	s.Require().NotEmpty(got, "echo provider should return output")
	s.Assert().Contains(got, "HELLO_FROM_E2E")

	// Stop.
	_, err = client.StopSession(ctx, &bridgev1.StopSessionRequest{
		SessionId: sessionID,
		Force:     true,
	})
	s.Require().NoError(err)
}

// --- Tier-1 auto-PKI tests (Section 2 of test plan) ---

// TestAutoPKIGeneratesAllFiles verifies that starting a secure-mode server
// with no Step CA flags generates the full set of PKI files.
func (s *CLISuite) TestAutoPKIGeneratesAllFiles() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err, "secure server should start")
	defer srv.Stop()

	certsDir := filepath.Join(stateDir, "certs")
	expectedFiles := []string{
		"ca.crt",
		"ca.key",
		"server.crt",
		"server.key",
		"local-client.crt",
		"local-client.key",
		"ca-bundle.crt",
		"jwt-signing.key",
		"jwt-signing.pub",
	}
	for _, name := range expectedFiles {
		path := filepath.Join(certsDir, name)
		_, err := os.Stat(path)
		s.Assert().NoError(err, "PKI file should exist: %s", name)
	}

	// Private keys should have restricted permissions (0600).
	privateKeys := []string{"ca.key", "server.key", "local-client.key", "jwt-signing.key"}
	for _, name := range privateKeys {
		info, err := os.Stat(filepath.Join(certsDir, name))
		s.Require().NoError(err)
		s.Assert().Equal(os.FileMode(0o600), info.Mode().Perm(),
			"private key %s should be 0600", name)
	}

	// Health check should succeed with the auto-generated creds.
	target, _ := localserver.DiscoverTarget(stateDir)
	client := s.secureClient(target, stateDir)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Health(ctx)
	s.Require().NoError(err, "health check should succeed with auto-PKI creds")
	s.Assert().NotEmpty(resp.ServerInstanceId)
}

// TestAutoPKIIdempotentAcrossRestart verifies that stopping and restarting
// a secure-mode server does not regenerate certificates.
func (s *CLISuite) TestAutoPKIIdempotentAcrossRestart() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	// First start — generates PKI.
	srv1, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)

	certsDir := filepath.Join(stateDir, "certs")
	bundlePath := filepath.Join(certsDir, "ca-bundle.crt")
	caPath := filepath.Join(certsDir, "ca.crt")

	// Record contents from first start.
	bundle1, err := os.ReadFile(bundlePath)
	s.Require().NoError(err)
	ca1, err := os.ReadFile(caPath)
	s.Require().NoError(err)

	srv1.Stop()

	// Second start — should reuse existing certs.
	srv2, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)
	defer srv2.Stop()

	bundle2, err := os.ReadFile(bundlePath)
	s.Require().NoError(err)
	ca2, err := os.ReadFile(caPath)
	s.Require().NoError(err)

	s.Assert().Equal(bundle1, bundle2, "ca-bundle.crt should not be regenerated")
	s.Assert().Equal(ca1, ca2, "ca.crt should not be regenerated")

	// Server should still be fully functional with the original certs.
	target, _ := localserver.DiscoverTarget(stateDir)
	client := s.secureClient(target, stateDir)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Health(ctx)
	s.Require().NoError(err, "health check should pass after restart with same certs")
	s.Assert().NotEmpty(resp.ServerInstanceId)
}

// TestIssuedClientCertCanConnect verifies that a client using credentials
// from IssueClientCert can connect to and authenticate with the server.
func (s *CLISuite) TestIssuedClientCertCanConnect() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Start and stop a server to generate PKI.
	srv1, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)
	srv1.Stop()

	// Issue a client certificate. This writes the JWT public key to
	// certs/jwt-clients/ which the server reads at startup.
	clientName := "sdk-test"
	certPath, keyPath, err := localserver.IssueClientCert(stateDir, clientName, logger)
	s.Require().NoError(err, "should issue client cert")

	// Verify expected files were created.
	clientDir := filepath.Join(stateDir, "certs", "clients", clientName)
	s.Assert().Equal(filepath.Join(clientDir, clientName+".crt"), certPath)
	s.Assert().Equal(filepath.Join(clientDir, clientName+".key"), keyPath)
	_, err = os.Stat(filepath.Join(clientDir, "jwt-signing.key"))
	s.Require().NoError(err, "per-client JWT key should exist")
	_, err = os.Stat(filepath.Join(stateDir, "certs", "jwt-clients", clientName+".pub"))
	s.Require().NoError(err, "server-side JWT pub should be registered")

	// Restart server so it loads the new JWT public key.
	srv2, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)
	defer srv2.Stop()

	target, _ := localserver.DiscoverTarget(stateDir)

	// Connect using the issued client credentials (not local-client).
	mat := localserver.LoadPKIMaterial(stateDir)
	issuedClient, err := bridgeclient.New(
		bridgeclient.WithTarget(target),
		bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: mat.CABundlePath,
			CertPath:     certPath,
			KeyPath:      keyPath,
			ServerName:   "server",
		}),
		bridgeclient.WithJWT(bridgeclient.JWTConfig{
			PrivateKeyPath: filepath.Join(clientDir, "jwt-signing.key"),
			Issuer:         clientName,
			Audience:       "bridge",
		}),
	)
	s.Require().NoError(err)
	defer func() { _ = issuedClient.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := issuedClient.Health(ctx)
	s.Require().NoError(err, "issued client should authenticate successfully")
	s.Assert().NotEmpty(resp.ServerInstanceId)
}

// TestClientNameValidation verifies that IssueClientCert rejects invalid
// names (path traversal, special characters) and accepts valid ones.
func (s *CLISuite) TestClientNameValidation() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Generate PKI so IssueClientCert has a CA to sign with.
	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)
	defer srv.Stop()

	// Invalid names should be rejected.
	invalidNames := []struct {
		name   string
		reason string
	}{
		{"../escape", "path traversal"},
		{"foo/bar", "slash in name"},
		{".hidden", "leading dot"},
		{"", "empty string"},
		{"a b c", "spaces"},
	}
	for _, tc := range invalidNames {
		_, _, err := localserver.IssueClientCert(stateDir, tc.name, logger)
		s.Assert().Error(err, "should reject %q (%s)", tc.name, tc.reason)
	}

	// Valid names should be accepted.
	validNames := []string{"a", "valid-client_1.0", "laptop2", "dev-machine", "server.local"}
	for _, name := range validNames {
		_, _, err := localserver.IssueClientCert(stateDir, name, logger)
		s.Assert().NoError(err, "should accept %q", name)
	}
}

// --- Step CA flag validation tests (Section 3 of test plan) ---

// TestStepCAMissingRoot verifies that starting a server with --step-ca-url
// but without --step-ca-root returns a clear error.
func (s *CLISuite) TestStepCAMissingRoot() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	_, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
		StepCAURL:  "https://ca.example.com",
		// StepCARootPath intentionally omitted
	})
	s.Require().Error(err, "should fail when --step-ca-root is missing")
	s.Assert().Contains(err.Error(), "step-ca-root is required",
		"error should mention the missing flag")
}

// TestStepCANonexistentRoot verifies that a nonexistent --step-ca-root path
// produces a clear error about the missing file.
func (s *CLISuite) TestStepCANonexistentRoot() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	_, err := localserver.Start(localserver.Config{
		StateDir:       stateDir,
		ListenAddr:     "127.0.0.1:0",
		ServerSANs:     []string{"127.0.0.1"},
		StepCAURL:      "https://ca.example.com",
		StepCARootPath: "/nonexistent/root.crt",
	})
	s.Require().Error(err, "should fail when root cert file does not exist")
	s.Assert().Contains(err.Error(), "copy Step CA root",
		"error should mention the copy failure")
}

// TestStepCAMissingStepCLI verifies that when `step` is not on PATH,
// the server returns a clear error with an install link.
func (s *CLISuite) TestStepCAUnreachableCA() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	// Create a dummy root cert file so the copy step succeeds.
	dummyRoot := filepath.Join(s.T().TempDir(), "root.crt")
	s.Require().NoError(os.WriteFile(dummyRoot, []byte("dummy-cert"), 0o644))

	_, err := localserver.Start(localserver.Config{
		StateDir:       stateDir,
		ListenAddr:     "127.0.0.1:0",
		ServerSANs:     []string{"127.0.0.1"},
		StepCAURL:      "https://ca.example.com",
		StepCARootPath: dummyRoot,
	})
	s.Require().Error(err, "should fail when Step CA is unreachable")
	s.Assert().Contains(err.Error(), "Step CA",
		"error should mention Step CA")
}

// TestOIDCFlagValidation verifies that IssueClientCertViaOIDC rejects
// incomplete flag combinations with clear error messages.
func (s *CLISuite) TestOIDCFlagValidation() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	tests := []struct {
		name    string
		client  string
		stepCA  *localserver.StepCAConfig
		wantErr string
	}{
		{
			name:    "missing step-ca-url",
			client:  "alice",
			stepCA:  nil,
			wantErr: "step-ca-url",
		},
		{
			name:   "missing oidc-provider",
			client: "alice",
			stepCA: &localserver.StepCAConfig{
				URL:      "https://ca.example.com",
				RootPath: "/tmp/root.crt",
			},
			wantErr: "oidc-provider",
		},
		{
			name:   "missing step-ca-root",
			client: "alice",
			stepCA: &localserver.StepCAConfig{
				URL:             "https://ca.example.com",
				OIDCProviderURL: "https://accounts.google.com",
			},
			wantErr: "step-ca-root",
		},
		{
			name:   "invalid client name",
			client: "../escape",
			stepCA: &localserver.StepCAConfig{
				URL:             "https://ca.example.com",
				RootPath:        "/tmp/root.crt",
				OIDCProviderURL: "https://accounts.google.com",
			},
			wantErr: "invalid client name",
		},
		{
			name:   "step CLI not on PATH",
			client: "bob",
			stepCA: &localserver.StepCAConfig{
				URL:             "https://ca.example.com",
				RootPath:        "/tmp/root.crt",
				OIDCProviderURL: "https://accounts.google.com",
			},
			wantErr: "step",
		},
	}

	for _, tc := range tests {
		s.Run(tc.name, func() {
			// Ensure step is not on PATH for the last test case.
			if tc.name == "step CLI not on PATH" {
				s.T().Setenv("PATH", s.T().TempDir())
			}
			_, _, err := localserver.IssueClientCertViaOIDC(stateDir, tc.client, tc.stepCA, logger)
			s.Require().Error(err, "should fail for case: %s", tc.name)
			s.Assert().Contains(err.Error(), tc.wantErr,
				"error for %q should mention %q", tc.name, tc.wantErr)
		})
	}
}

// TestOIDCMissingNameFlag verifies that the CLI rejects issue-client --oidc-provider
// when --name is not provided (Cobra flag validation).
func (s *CLISuite) TestOIDCMissingNameFlag() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	cmd := exec.Command(cliBinary, "server", "issue-client",
		"--oidc-provider", "https://accounts.google.com",
		"--step-ca-url", "https://ca.example.com",
		"--step-ca-root", "/tmp/root.crt",
	)
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	s.Require().Error(err, "should fail when --name is missing")
	s.Assert().Contains(stderr.String(), "name",
		"error should mention the missing --name flag")
}

// --- Writer slot release tests (Section 4 of test plan) ---

// startEchoSession creates a session using the echo provider and waits for it
// to reach the running state. It returns the session ID.
func (s *CLISuite) startEchoSession(ctx context.Context, client *bridgeclient.Client, repoDir string) string {
	s.T().Helper()
	sessionID := uuid.NewString()
	_, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "test",
		SessionId:   sessionID,
		RepoPath:    repoDir,
		Provider:    "echo",
		InitialCols: 80,
		InitialRows: 24,
	})
	s.Require().NoError(err)
	// Wait for session to be running.
	time.Sleep(500 * time.Millisecond)
	return sessionID
}

// attachAndCollectEvents attaches to a session and collects events in the
// background. Returns the stream, a channel that signals when ATTACHED is
// received, and a function to retrieve collected events.
func (s *CLISuite) attachAndCollectEvents(
	ctx context.Context,
	client *bridgeclient.Client,
	sessionID, clientID string,
	role bridgev1.AttachRole,
) (stream *bridgeclient.OutputStream, attached <-chan struct{}, getEvents func() []*bridgev1.AttachSessionEvent, cancel context.CancelFunc) {
	s.T().Helper()
	recvCtx, recvCancel := context.WithCancel(ctx)

	stream, err := client.AttachSession(recvCtx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  clientID,
		AfterSeq:  0,
		Role:      role,
	})
	s.Require().NoError(err)

	var mu sync.Mutex
	var events []*bridgev1.AttachSessionEvent
	attachedCh := make(chan struct{}, 1)

	go func() {
		_ = stream.RecvAll(recvCtx, func(ev *bridgev1.AttachSessionEvent) error {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED {
				select {
				case attachedCh <- struct{}{}:
				default:
				}
			}
			return nil
		})
	}()

	return stream, attachedCh, func() []*bridgev1.AttachSessionEvent {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]*bridgev1.AttachSessionEvent, len(events))
		copy(cp, events)
		return cp
	}, recvCancel
}

// waitForAttach blocks until the attached channel fires or a timeout expires.
func (s *CLISuite) waitForAttach(attached <-chan struct{}) {
	s.T().Helper()
	select {
	case <-attached:
	case <-time.After(3 * time.Second):
		s.T().Fatal("timeout waiting for attach event")
	}
}

// hasEventType returns true if the event slice contains an event of the given type.
func hasEventType(events []*bridgev1.AttachSessionEvent, typ bridgev1.AttachEventType) bool {
	for _, ev := range events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// TestWriterReleasedOnDisconnect verifies that when the active writer
// disconnects, observers receive a WRITER_RELEASED event.
func (s *CLISuite) TestWriterReleasedOnDisconnect() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{StateDir: stateDir})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Target()
	ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ctxCancel()

	// Create two clients sharing the same gRPC connection.
	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	sessionID := s.startEchoSession(ctx, client, repoDir)

	// Client A: attach as writer.
	writerID := uuid.NewString()
	_, writerAttached, _, writerCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, writerID,
		bridgev1.AttachRole_ATTACH_ROLE_WRITER,
	)
	s.waitForAttach(writerAttached)

	// Client B: attach as observer.
	observerID := uuid.NewString()
	_, observerAttached, getObserverEvents, observerCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, observerID,
		bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
	)
	defer observerCancel()
	s.waitForAttach(observerAttached)

	// Writer disconnects — cancel its stream context.
	writerCancel()
	// Give the server time to broadcast the WRITER_RELEASED event.
	time.Sleep(500 * time.Millisecond)

	// Observer should have received WRITER_RELEASED.
	events := getObserverEvents()
	s.Assert().True(hasEventType(events, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED),
		"observer should receive WRITER_RELEASED when writer disconnects; got events: %v", eventTypes(events))

	// Verify the WRITER_RELEASED event identifies the disconnected writer.
	for _, ev := range events {
		if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED {
			s.Assert().Equal(writerID, ev.WriterClientId,
				"WRITER_RELEASED should identify the disconnected writer")
		}
	}

	// Cleanup.
	_, _ = client.StopSession(ctx, &bridgev1.StopSessionRequest{
		SessionId: sessionID, Force: true,
	})
}

// TestWriterEvictionBroadcastsEvents verifies that force-claiming the writer
// slot broadcasts WRITER_RELEASED (for the evicted writer) and WRITER_CLAIMED
// (for the new writer) to all observers.
func (s *CLISuite) TestWriterEvictionBroadcastsEvents() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{StateDir: stateDir})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Target()
	ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ctxCancel()

	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	sessionID := s.startEchoSession(ctx, client, repoDir)

	// Client A: attach as writer.
	writerAID := uuid.NewString()
	_, writerAAttached, getWriterAEvents, writerACancel := s.attachAndCollectEvents(
		ctx, client, sessionID, writerAID,
		bridgev1.AttachRole_ATTACH_ROLE_WRITER,
	)
	defer writerACancel()
	s.waitForAttach(writerAAttached)

	// Client B: attach as observer.
	observerID := uuid.NewString()
	_, observerAttached, getObserverEvents, observerCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, observerID,
		bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
	)
	defer observerCancel()
	s.waitForAttach(observerAttached)

	// Client C: attach as observer first (ClaimWriter requires an attached client).
	claimantID := uuid.NewString()
	_, claimantAttached, _, claimantCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, claimantID,
		bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
	)
	defer claimantCancel()
	s.waitForAttach(claimantAttached)

	// Client C: force-claim the writer slot, evicting Client A.
	claimResp, err := client.ClaimWriter(ctx, &bridgev1.ClaimWriterRequest{
		SessionId: sessionID,
		ClientId:  claimantID,
		Force:     true,
	})
	s.Require().NoError(err)
	s.Assert().True(claimResp.Claimed, "force claim should succeed")
	s.Assert().Equal(writerAID, claimResp.PreviousWriterClientId,
		"should report the evicted writer")

	// Give the server time to broadcast events.
	time.Sleep(500 * time.Millisecond)

	// Observer (Client B) should see both WRITER_RELEASED and WRITER_CLAIMED.
	obsEvents := getObserverEvents()
	s.Assert().True(hasEventType(obsEvents, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED),
		"observer should receive WRITER_RELEASED for the evicted writer; got: %v", eventTypes(obsEvents))
	s.Assert().True(hasEventType(obsEvents, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_CLAIMED),
		"observer should receive WRITER_CLAIMED for the new writer; got: %v", eventTypes(obsEvents))

	// Verify the event payloads identify the correct clients.
	for _, ev := range obsEvents {
		switch ev.Type {
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED:
			s.Assert().Equal(writerAID, ev.WriterClientId,
				"WRITER_RELEASED should identify the evicted writer")
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_CLAIMED:
			s.Assert().Equal(claimantID, ev.WriterClientId,
				"WRITER_CLAIMED should identify the new writer")
		}
	}

	// Client A (evicted writer, now observer) should also see the events.
	writerAEvents := getWriterAEvents()
	s.Assert().True(hasEventType(writerAEvents, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED),
		"evicted writer should receive WRITER_RELEASED; got: %v", eventTypes(writerAEvents))
	s.Assert().True(hasEventType(writerAEvents, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_CLAIMED),
		"evicted writer should receive WRITER_CLAIMED; got: %v", eventTypes(writerAEvents))

	// Cleanup.
	_, _ = client.StopSession(ctx, &bridgev1.StopSessionRequest{
		SessionId: sessionID, Force: true,
	})
}

// TestObserverClaimsWriterAfterRelease verifies that an observer can claim the
// writer slot after it is voluntarily released, and then write input.
func (s *CLISuite) TestObserverClaimsWriterAfterRelease() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{StateDir: stateDir})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Target()
	ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ctxCancel()

	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	sessionID := s.startEchoSession(ctx, client, repoDir)

	// Client A: attach as writer.
	writerAID := uuid.NewString()
	_, writerAAttached, _, writerACancel := s.attachAndCollectEvents(
		ctx, client, sessionID, writerAID,
		bridgev1.AttachRole_ATTACH_ROLE_WRITER,
	)
	defer writerACancel()
	s.waitForAttach(writerAAttached)

	// Client B: attach as observer.
	observerID := uuid.NewString()
	observerStream, observerAttached, getObserverEvents, observerCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, observerID,
		bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
	)
	defer observerCancel()
	s.waitForAttach(observerAttached)

	// Client A: voluntarily release the writer slot.
	releaseResp, err := client.ReleaseWriter(ctx, &bridgev1.ReleaseWriterRequest{
		SessionId: sessionID,
		ClientId:  writerAID,
	})
	s.Require().NoError(err)
	s.Assert().True(releaseResp.Released, "release should succeed")

	// Give the server time to broadcast WRITER_RELEASED.
	time.Sleep(300 * time.Millisecond)

	// Observer should have received the release notification.
	events := getObserverEvents()
	s.Assert().True(hasEventType(events, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED),
		"observer should see WRITER_RELEASED after voluntary release; got: %v", eventTypes(events))

	// Client B (observer): claim the now-vacant writer slot (force=false).
	claimResp, err := client.ClaimWriter(ctx, &bridgev1.ClaimWriterRequest{
		SessionId: sessionID,
		ClientId:  observerID,
		Force:     false,
	})
	s.Require().NoError(err, "non-force claim should succeed when slot is vacant")
	s.Assert().True(claimResp.Claimed)

	// Client B should now be able to write input.
	_, err = client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  observerStream.ClientID(),
		Data:      []byte("OBSERVER_NOW_WRITER\n"),
	})
	s.Require().NoError(err, "promoted observer should be able to write input")

	// Cleanup.
	_, _ = client.StopSession(ctx, &bridgev1.StopSessionRequest{
		SessionId: sessionID, Force: true,
	})
}

// eventTypes returns a slice of event type names for diagnostic output.
func eventTypes(events []*bridgev1.AttachSessionEvent) []string {
	names := make([]string, len(events))
	for i, ev := range events {
		names[i] = ev.Type.String()
	}
	return names
}

// --- Re-attachment and terminal tests (Section 5 of test plan) ---

// TestSameClientIDReattachment verifies that when a client re-attaches with the
// same clientID, the stale channel is closed and the new attachment works. This
// exercises the re-attachment safety code that prevents goroutine leaks.
//
// The re-attachment path triggers when the same clientID attaches while an
// existing observer entry is still present. The writer conflict check runs
// first, so a realistic reconnect re-attaches as observer and then reclaims
// the writer slot.
func (s *CLISuite) TestSameClientIDReattachment() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{StateDir: stateDir})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Target()
	ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ctxCancel()

	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	sessionID := s.startEchoSession(ctx, client, repoDir)

	// First attachment as observer with a fixed clientID.
	fixedClientID := "reattach-test-client"
	_, firstAttached, _, firstCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, fixedClientID,
		bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
	)
	s.waitForAttach(firstAttached)

	// Second attachment with the SAME clientID as observer — the server
	// closes the stale channel and registers the new one.
	_, secondAttached, getSecondEvents, secondCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, fixedClientID,
		bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
	)
	s.waitForAttach(secondAttached)

	// The first stream's context is still live, but the server closed its
	// channel. Cancel it to clean up the goroutine.
	firstCancel()

	// The second attachment should be functional — verify it received the
	// ATTACHED event.
	events := getSecondEvents()
	s.Assert().True(hasEventType(events, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED),
		"re-attached stream should receive ATTACHED event")

	secondCancel()

	// Cleanup.
	_, _ = client.StopSession(ctx, &bridgev1.StopSessionRequest{
		SessionId: sessionID, Force: true,
	})
}

// TestReattachmentAsObserverThenClaimWriter verifies that a client can
// re-attach as observer after being a writer, and then reclaim the writer slot.
func (s *CLISuite) TestReattachmentAsObserverThenClaimWriter() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{StateDir: stateDir})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Target()
	ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ctxCancel()

	client, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	sessionID := s.startEchoSession(ctx, client, repoDir)

	clientID := "role-switch-client"

	// Attach as writer.
	_, writerAttached, _, writerCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, clientID,
		bridgev1.AttachRole_ATTACH_ROLE_WRITER,
	)
	s.waitForAttach(writerAttached)

	// Re-attach the same clientID as observer. The server closes the stale
	// writer channel and re-registers as observer.
	_, observerAttached, _, observerCancel := s.attachAndCollectEvents(
		ctx, client, sessionID, clientID,
		bridgev1.AttachRole_ATTACH_ROLE_OBSERVER,
	)
	s.waitForAttach(observerAttached)
	writerCancel() // clean up old stream goroutine

	// The writer slot should now be vacant since re-attach cleared it.
	// Claim it back with force=false (should succeed on an empty slot).
	claimResp, err := client.ClaimWriter(ctx, &bridgev1.ClaimWriterRequest{
		SessionId: sessionID,
		ClientId:  clientID,
		Force:     false,
	})
	s.Require().NoError(err, "claim should succeed when writer slot is vacant")
	s.Assert().True(claimResp.Claimed)

	// Verify we can write input as the reclaimed writer.
	_, err = client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  clientID,
		Data:      []byte("RECLAIMED\n"),
	})
	s.Require().NoError(err, "write should succeed after reclaiming writer slot")

	observerCancel()
	_, _ = client.StopSession(ctx, &bridgev1.StopSessionRequest{
		SessionId: sessionID, Force: true,
	})
}

// --- Tier-2 Step CA integration tests (Section 6 of test plan) ---
//
// These tests use a fake `step` CLI binary so the Step CA code path can be
// exercised without a real Step CA server. The fake binary writes placeholder
// cert/key files. Because the fake certs are not valid TLS material, these
// tests call EnsurePKI and IssueClientCert directly instead of doing a full
// localserver.Start() + health check (which would require real certs).

// fakeStepBin writes a stub `step` binary into a temp directory and prepends
// that directory to PATH. The stub writes placeholder files for
// `step ca certificate <name> <cert> <key> ...`.
func (s *CLISuite) fakeStepBin() {
	s.T().Helper()
	if runtime.GOOS == "windows" {
		s.T().Skip("fake step script not supported on Windows")
	}
	dir := s.T().TempDir()
	script := `#!/bin/sh
# Fake step CLI: write placeholder cert/key for "step ca certificate <name> <cert> <key> ..."
if [ "$1" = "ca" ] && [ "$2" = "certificate" ]; then
  echo "fake-cert" > "$4"
  echo "fake-key"  > "$5"
fi
exit 0
`
	scriptPath := filepath.Join(dir, "step")
	s.Require().NoError(os.WriteFile(scriptPath, []byte(script), 0o755))
	s.T().Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// mockCertRequester overrides the native JWK/ACME cert request functions with
// a stub that writes placeholder cert/key files. This replaces fakeStepBin for
// tests that call EnsurePKI with a StepCAConfig.
func (s *CLISuite) mockCertRequester() {
	s.T().Helper()
	mock := func(_ *localserver.StepCAConfig, _ []string, certPath, keyPath string, _ *slog.Logger) error {
		if err := os.WriteFile(certPath, []byte("fake-cert"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(keyPath, []byte("fake-key"), 0o600)
	}
	restore := localserver.SetCertRequestFuncs(mock, mock)
	s.T().Cleanup(restore)
}

// TestStepCADualCAArchitecture verifies that EnsurePKI with Step CA config
// creates both Step CA-derived server certs and a local CA for CLI credentials,
// and that the ca-bundle.crt contains both trust roots.
func (s *CLISuite) TestStepCADualCAArchitecture() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	s.mockCertRequester()
	stateDir := s.testStateDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Write a fake Step CA root cert.
	rootPEM := filepath.Join(s.T().TempDir(), "step-ca-root.crt")
	s.Require().NoError(os.WriteFile(rootPEM, []byte("step-ca-root-cert"), 0o644))

	stepCfg := &localserver.StepCAConfig{
		URL:      "https://ca.example.internal:443",
		RootPath: rootPEM,
	}
	mat, err := localserver.EnsurePKI(stateDir, []string{"10.0.0.1"}, logger, stepCfg, 0)
	s.Require().NoError(err)

	// 1. ca-bundle.crt should contain the Step CA root first, then the local CA.
	bundle, err := os.ReadFile(mat.CABundlePath)
	s.Require().NoError(err)
	bundleStr := string(bundle)
	s.Assert().True(strings.HasPrefix(bundleStr, "step-ca-root-cert"),
		"bundle should start with Step CA root")
	s.Assert().Contains(bundleStr, "BEGIN CERTIFICATE",
		"bundle should also contain local CA cert (PEM)")

	// 2. Local CA should exist and be self-signed.
	_, err = os.Stat(mat.CACertPath)
	s.Assert().NoError(err, "local CA cert should exist")
	_, err = os.Stat(mat.CAKeyPath)
	s.Assert().NoError(err, "local CA key should exist")

	// 3. Server cert and key should exist (from fake step binary).
	_, err = os.Stat(mat.ServerCertPath)
	s.Assert().NoError(err, "server cert should exist")
	_, err = os.Stat(mat.ServerKeyPath)
	s.Assert().NoError(err, "server key should exist")

	// 4. Local-client cert should exist (for bridgectl CLI).
	_, err = os.Stat(mat.LocalClientCert)
	s.Assert().NoError(err, "local-client cert should exist")
	_, err = os.Stat(mat.LocalClientKey)
	s.Assert().NoError(err, "local-client key should exist")

	// 5. JWT keypair should be auto-generated.
	_, err = os.Stat(mat.JWTSigningPub)
	s.Assert().NoError(err, "JWT pub should exist")
	_, err = os.Stat(mat.JWTSigningKey)
	s.Assert().NoError(err, "JWT key should exist")
}

// TestStepCAIdempotency verifies that a second EnsurePKI call with Step CA
// config is a no-op when ca-bundle.crt already exists.
func (s *CLISuite) TestStepCAIdempotency() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	s.mockCertRequester()
	// Stub mTLS so the renewal fallback path works when
	// ensureStepCACertFresh encounters an unreadable fake cert.
	restoreMTLS := localserver.SetCertRenewerFunc(
		func(_ *localserver.StepCAConfig, _, _ string, _ *slog.Logger) error {
			return fmt.Errorf("cert unreadable")
		},
	)
	s.T().Cleanup(restoreMTLS)

	stateDir := s.testStateDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	rootPEM := filepath.Join(s.T().TempDir(), "step-ca-root.crt")
	s.Require().NoError(os.WriteFile(rootPEM, []byte("original-root"), 0o644))

	pwFile := filepath.Join(s.T().TempDir(), "password")
	s.Require().NoError(os.WriteFile(pwFile, []byte("test"), 0o600))

	stepCfg := &localserver.StepCAConfig{
		URL:                     "https://ca.example.internal:443",
		RootPath:                rootPEM,
		ProvisionerPasswordFile: pwFile,
	}

	// First call — generates everything.
	mat1, err := localserver.EnsurePKI(stateDir, []string{"10.0.0.1"}, logger, stepCfg, 0)
	s.Require().NoError(err)
	bundle1, err := os.ReadFile(mat1.CABundlePath)
	s.Require().NoError(err)

	// Overwrite root with different content.
	s.Require().NoError(os.WriteFile(rootPEM, []byte("changed-root"), 0o644))

	// Second call — should be no-op; bundle should retain original content.
	mat2, err := localserver.EnsurePKI(stateDir, []string{"10.0.0.1"}, logger, stepCfg, 0)
	s.Require().NoError(err)
	bundle2, err := os.ReadFile(mat2.CABundlePath)
	s.Require().NoError(err)

	s.Assert().Equal(bundle1, bundle2,
		"ca-bundle.crt should not be regenerated on second call")
	s.Assert().True(strings.HasPrefix(string(bundle2), "original-root"),
		"bundle should retain the original Step CA root")
	s.Assert().NotContains(string(bundle2), "changed-root",
		"bundle should not reflect the overwritten root file")
}

// TestStepCATier1ClientIssuance verifies that Tier-1 client certificate
// issuance (IssueClientCert, no OIDC) works correctly when the server PKI
// was initialized with Step CA — the local CA signs the client cert.
func (s *CLISuite) TestStepCATier1ClientIssuance() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	s.mockCertRequester()
	stateDir := s.testStateDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	rootPEM := filepath.Join(s.T().TempDir(), "step-ca-root.crt")
	s.Require().NoError(os.WriteFile(rootPEM, []byte("step-ca-root-cert"), 0o644))

	stepCfg := &localserver.StepCAConfig{
		URL:      "https://ca.example.internal:443",
		RootPath: rootPEM,
	}

	// Initialize PKI with Step CA.
	_, err := localserver.EnsurePKI(stateDir, []string{"10.0.0.1"}, logger, stepCfg, 0)
	s.Require().NoError(err)

	// Issue a Tier-1 client cert (signed by local CA, not Step CA).
	clientName := "sdk-client"
	certPath, keyPath, err := localserver.IssueClientCert(stateDir, clientName, logger)
	s.Require().NoError(err, "Tier-1 client issuance should work with Step CA PKI")

	// Verify expected file layout.
	clientDir := filepath.Join(stateDir, "certs", "clients", clientName)
	s.Assert().Equal(filepath.Join(clientDir, clientName+".crt"), certPath)
	s.Assert().Equal(filepath.Join(clientDir, clientName+".key"), keyPath)

	// Per-client JWT key should exist.
	_, err = os.Stat(filepath.Join(clientDir, "jwt-signing.key"))
	s.Require().NoError(err, "per-client JWT key should exist")

	// Server-side JWT pub key should be registered.
	_, err = os.Stat(filepath.Join(stateDir, "certs", "jwt-clients", clientName+".pub"))
	s.Require().NoError(err, "server-side JWT pub key should be registered")
}

// --- OIDC client enrollment tests (Section 7 of test plan) ---
//
// These tests use the fake `step` CLI binary. Real OIDC enrollment requires
// an interactive browser login, so these verify the file layout, JWT isolation,
// and credential structure without a live Step CA + OIDC provider.

// TestOIDCEnrollmentHappyPath verifies that IssueClientCertViaOIDC creates
// the expected file layout (cert, key, JWT keypair) and registers the
// server-side JWT public key.
func (s *CLISuite) TestOIDCEnrollmentHappyPath() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	s.fakeStepBin()
	stateDir := s.testStateDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	rootPEM := filepath.Join(s.T().TempDir(), "step-ca-root.crt")
	s.Require().NoError(os.WriteFile(rootPEM, []byte("fake-root"), 0o644))

	stepCfg := &localserver.StepCAConfig{
		URL:             "https://ca.example.internal:443",
		RootPath:        rootPEM,
		OIDCProviderURL: "https://accounts.google.com",
	}

	clientName := "human1"
	certPath, keyPath, err := localserver.IssueClientCertViaOIDC(stateDir, clientName, stepCfg, logger)
	s.Require().NoError(err, "OIDC enrollment should succeed with fake step")

	// Verify expected file layout.
	clientDir := filepath.Join(stateDir, "certs", "clients", clientName)
	s.Assert().Equal(filepath.Join(clientDir, clientName+".crt"), certPath)
	s.Assert().Equal(filepath.Join(clientDir, clientName+".key"), keyPath)

	// Cert and key files should exist (written by fake step).
	_, err = os.Stat(certPath)
	s.Assert().NoError(err, "client cert should exist")
	_, err = os.Stat(keyPath)
	s.Assert().NoError(err, "client key should exist")

	// Per-client JWT keypair should be generated locally.
	_, err = os.Stat(filepath.Join(clientDir, "jwt-signing.key"))
	s.Assert().NoError(err, "per-client JWT signing key should exist")

	// Server-side JWT public key should be registered.
	serverPub := filepath.Join(stateDir, "certs", "jwt-clients", clientName+".pub")
	_, err = os.Stat(serverPub)
	s.Assert().NoError(err, "server-side JWT pub key should be registered")
}

// TestOIDCPerClientJWTIsolation verifies that two OIDC-enrolled clients get
// independent JWT keypairs — each client's jwt-signing.key is unique, and each
// has its own entry in jwt-clients/.
func (s *CLISuite) TestOIDCPerClientJWTIsolation() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	s.fakeStepBin()
	stateDir := s.testStateDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	rootPEM := filepath.Join(s.T().TempDir(), "step-ca-root.crt")
	s.Require().NoError(os.WriteFile(rootPEM, []byte("fake-root"), 0o644))

	stepCfg := &localserver.StepCAConfig{
		URL:             "https://ca.example.internal:443",
		RootPath:        rootPEM,
		OIDCProviderURL: "https://accounts.google.com",
	}

	// Enroll two clients.
	_, _, err := localserver.IssueClientCertViaOIDC(stateDir, "human1", stepCfg, logger)
	s.Require().NoError(err)
	_, _, err = localserver.IssueClientCertViaOIDC(stateDir, "human2", stepCfg, logger)
	s.Require().NoError(err)

	// Each client should have its own JWT signing key.
	key1, err := os.ReadFile(filepath.Join(stateDir, "certs", "clients", "human1", "jwt-signing.key"))
	s.Require().NoError(err)
	key2, err := os.ReadFile(filepath.Join(stateDir, "certs", "clients", "human2", "jwt-signing.key"))
	s.Require().NoError(err)
	s.Assert().NotEqual(key1, key2, "each client should have a unique JWT signing key")

	// Each client should have its own server-side JWT public key.
	pub1, err := os.ReadFile(filepath.Join(stateDir, "certs", "jwt-clients", "human1.pub"))
	s.Require().NoError(err)
	pub2, err := os.ReadFile(filepath.Join(stateDir, "certs", "jwt-clients", "human2.pub"))
	s.Require().NoError(err)
	s.Assert().NotEqual(pub1, pub2, "each client should have a unique JWT public key")

	// Both public keys should be registered.
	_, err = os.Stat(filepath.Join(stateDir, "certs", "jwt-clients", "human1.pub"))
	s.Assert().NoError(err)
	_, err = os.Stat(filepath.Join(stateDir, "certs", "jwt-clients", "human2.pub"))
	s.Assert().NoError(err)
}

// --- Secure mode tests ---

// secureClient creates a bridgeclient connected to a secure-mode server
// using the auto-generated local-client credentials.
func (s *CLISuite) secureClient(target, stateDir string) *bridgeclient.Client {
	s.T().Helper()
	mat := localserver.LoadPKIMaterial(stateDir)
	client, err := bridgeclient.New(
		bridgeclient.WithTarget(target),
		bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: mat.CABundlePath,
			CertPath:     mat.LocalClientCert,
			KeyPath:      mat.LocalClientKey,
			ServerName:   "server",
		}),
		bridgeclient.WithJWT(bridgeclient.JWTConfig{
			PrivateKeyPath: mat.JWTSigningKey,
			Issuer:         "local",
			Audience:       "bridge",
		}),
	)
	s.Require().NoError(err, "secure client should connect")
	return client
}

// TestSecureModeStartStop verifies that the server starts and stops
// cleanly in secure (mTLS+JWT) mode.
func (s *CLISuite) TestSecureModeStartStop() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err, "secure server should start")
	defer srv.Stop()

	// Verify mode file says "secure".
	mode := localserver.DiscoverMode(stateDir)
	s.Assert().Equal(localserver.ModeSecure, mode)

	// Verify server is discoverable.
	target, discoveredMode := localserver.DiscoverTarget(stateDir)
	s.Require().NotEmpty(target, "should discover running secure server")
	s.Assert().Equal(localserver.ModeSecure, discoveredMode)

	// Health check via mTLS client.
	client := s.secureClient(target, stateDir)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Health(ctx)
	s.Require().NoError(err, "health check should succeed with mTLS")
	s.Assert().NotEmpty(resp.ServerInstanceId)

	// Stop server.
	srv.Stop()

	// Verify server is no longer discoverable.
	s.Assert().False(localserver.IsServerRunning(stateDir))
}

// TestSecureModeSessionLifecycle tests creating, listing, and stopping a
// session on a secure-mode server.
func (s *CLISuite) TestSecureModeSessionLifecycle() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()
	repoDir := s.T().TempDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)
	defer srv.Stop()

	target, _ := localserver.DiscoverTarget(stateDir)
	client := s.secureClient(target, stateDir)
	defer func() { _ = client.Close() }()
	client.SetProject("test")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start a session.
	sessionID := uuid.NewString()
	startResp, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "test",
		SessionId:   sessionID,
		RepoPath:    repoDir,
		Provider:    "echo",
		InitialCols: 80,
		InitialRows: 24,
	})
	s.Require().NoError(err)
	s.Assert().Equal(sessionID, startResp.SessionId)

	// Wait for session to start.
	var info *bridgev1.GetSessionResponse
	for i := 0; i < 20; i++ {
		info, err = client.GetSession(ctx, &bridgev1.GetSessionRequest{
			SessionId: sessionID,
		})
		if err == nil && info.Status != bridgev1.SessionStatus_SESSION_STATUS_STARTING {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Require().NoError(err)

	// List sessions.
	listResp, err := client.ListSessions(ctx, &bridgev1.ListSessionsRequest{
		ProjectId: "test",
	})
	s.Require().NoError(err)
	s.Assert().GreaterOrEqual(len(listResp.Sessions), 1)

	// Stop session.
	_, err = client.StopSession(ctx, &bridgev1.StopSessionRequest{
		SessionId: sessionID,
		Force:     true,
	})
	s.Require().NoError(err)
}

// TestSecureModeRejectsInsecureClient verifies that an insecure client
// cannot connect to a secure server.
func (s *CLISuite) TestSecureModeRejectsInsecureClient() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)
	defer srv.Stop()

	target, _ := localserver.DiscoverTarget(stateDir)
	s.Require().NotEmpty(target)

	// Try connecting without TLS — should fail.
	insecureClient, err := bridgeclient.New(bridgeclient.WithTarget(target))
	s.Require().NoError(err, "dial should succeed (lazy connection)")
	defer func() { _ = insecureClient.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = insecureClient.Health(ctx)
	s.Assert().Error(err, "insecure client should not be able to call secure server")
}

// TestSecureModeCleanup verifies that secure-mode state files (including
// server.mode) are cleaned up on stop.
func (s *CLISuite) TestSecureModeCleanup() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
	})
	s.Require().NoError(err)

	// Mode file should exist.
	_, err = os.Stat(filepath.Join(stateDir, "server.mode"))
	s.Assert().NoError(err, "mode file should exist while running")

	srv.Stop()

	// Mode file should be cleaned up.
	_, err = os.Stat(filepath.Join(stateDir, "server.mode"))
	s.Assert().True(os.IsNotExist(err), "mode file should be removed after stop")
}

// --- RegisterJWTKey enrollment tests (Section 8 of test plan) ---

// TestRegisterJWTKeyEnrollment verifies the full enrollment flow:
// 1. Start a secure server (auto-PKI)
// 2. Connect with mTLS only (no JWT) using the local-client cert
// 3. Generate a JWT keypair and call RegisterJWTKey
// 4. Reconnect with mTLS + JWT using the newly registered key
// 5. Verify an authenticated RPC succeeds
func (s *CLISuite) TestRegisterJWTKeyEnrollment() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err, "secure server should start")
	defer srv.Stop()

	target, _ := localserver.DiscoverTarget(stateDir)
	mat := localserver.LoadPKIMaterial(stateDir)

	// Step 1: Connect with mTLS only — no JWT. This simulates a client
	// that got its cert from Step CA but hasn't enrolled yet.
	mtlsOnlyClient, err := bridgeclient.New(
		bridgeclient.WithTarget(target),
		bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: mat.CABundlePath,
			CertPath:     mat.LocalClientCert,
			KeyPath:      mat.LocalClientKey,
			ServerName:   "server",
		}),
		bridgeclient.WithTimeout(5*time.Second),
	)
	s.Require().NoError(err)
	defer func() { _ = mtlsOnlyClient.Close() }()

	// Step 2: Generate a JWT keypair locally (like `client enroll` would).
	keyDir := s.T().TempDir()
	pubPath, privPath, err := pki.GenerateJWTKeypair(keyDir, "jwt-signing")
	s.Require().NoError(err)

	pubKey, err := pki.LoadEd25519PublicKey(pubPath)
	s.Require().NoError(err)
	pubDER, err := x509.MarshalPKIXPublicKey(pubKey)
	s.Require().NoError(err)

	// Step 3: Call RegisterJWTKey — issuer must match the client cert CN
	// ("local-client") to pass the identity check introduced to prevent
	// cross-issuer squatting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := mtlsOnlyClient.RegisterJWTKey(ctx, &bridgev1.RegisterJWTKeyRequest{
		PublicKey: pubDER,
		Issuer:    "local-client",
	})
	s.Require().NoError(err, "RegisterJWTKey should succeed with mTLS-only auth")
	s.Assert().Equal("local-client", resp.Issuer)

	// Verify the key was persisted to disk.
	_, err = os.Stat(filepath.Join(stateDir, "certs", "jwt-clients", "local-client.pub"))
	s.Require().NoError(err, "JWT public key should be persisted to disk")

	// Step 4: Connect with mTLS + JWT using the newly registered key.
	// No server restart needed — the key was hot-added.
	enrolledClient, err := bridgeclient.New(
		bridgeclient.WithTarget(target),
		bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: mat.CABundlePath,
			CertPath:     mat.LocalClientCert,
			KeyPath:      mat.LocalClientKey,
			ServerName:   "server",
		}),
		bridgeclient.WithJWT(bridgeclient.JWTConfig{
			PrivateKeyPath: privPath,
			Issuer:         "local-client",
			Audience:       "bridge",
		}),
		bridgeclient.WithTimeout(5*time.Second),
	)
	s.Require().NoError(err)
	defer func() { _ = enrolledClient.Close() }()

	// Step 5: Verify an authenticated RPC works with the enrolled key.
	enrolledClient.SetProject("test")
	healthResp, err := enrolledClient.Health(ctx)
	s.Require().NoError(err, "Health should succeed with enrolled JWT credentials")
	s.Assert().NotEmpty(healthResp.ServerInstanceId)

	// Also verify a JWT-protected RPC (ListSessions) works.
	_, err = enrolledClient.ListSessions(ctx, &bridgev1.ListSessionsRequest{
		ProjectId: "test",
	})
	s.Require().NoError(err, "ListSessions should succeed with enrolled JWT credentials")
}

// TestRegisterJWTKeyInsecureClientRejected verifies that an insecure client
// (no mTLS) cannot call RegisterJWTKey on a secure server.
func (s *CLISuite) TestRegisterJWTKeyInsecureClientRejected() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)
	defer srv.Stop()

	target, _ := localserver.DiscoverTarget(stateDir)

	// Try connecting without TLS — should fail at the transport level.
	insecureClient, err := bridgeclient.New(
		bridgeclient.WithTarget(target),
		bridgeclient.WithTimeout(3*time.Second),
	)
	s.Require().NoError(err, "dial should succeed (lazy connection)")
	defer func() { _ = insecureClient.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = insecureClient.RegisterJWTKey(ctx, &bridgev1.RegisterJWTKeyRequest{
		PublicKey: []byte("fake"),
		Issuer:    "attacker",
	})
	s.Assert().Error(err, "insecure client should not be able to call RegisterJWTKey")
}

// TestRegisterJWTKeySurvivesRestart verifies that a key registered via
// RegisterJWTKey persists across server restarts.
func (s *CLISuite) TestRegisterJWTKeySurvivesRestart() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	if runtime.GOOS == "windows" {
		s.T().Skip("secure mode not supported on Windows")
	}

	stateDir := s.testStateDir()

	// Start server, enroll a key, then stop.
	srv1, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)

	target1, _ := localserver.DiscoverTarget(stateDir)
	mat := localserver.LoadPKIMaterial(stateDir)

	// Generate and register a key.
	keyDir := s.T().TempDir()
	pubPath, privPath, err := pki.GenerateJWTKeypair(keyDir, "jwt-signing")
	s.Require().NoError(err)

	pubKey, err := pki.LoadEd25519PublicKey(pubPath)
	s.Require().NoError(err)
	pubDER, err := x509.MarshalPKIXPublicKey(pubKey)
	s.Require().NoError(err)

	mtlsClient, err := bridgeclient.New(
		bridgeclient.WithTarget(target1),
		bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: mat.CABundlePath,
			CertPath:     mat.LocalClientCert,
			KeyPath:      mat.LocalClientKey,
			ServerName:   "server",
		}),
		bridgeclient.WithTimeout(5*time.Second),
	)
	s.Require().NoError(err)

	// Issuer must match the local-client cert CN to pass the identity check.
	ctx := context.Background()
	_, err = mtlsClient.RegisterJWTKey(ctx, &bridgev1.RegisterJWTKeyRequest{
		PublicKey: pubDER,
		Issuer:    "local-client",
	})
	s.Require().NoError(err)
	_ = mtlsClient.Close()

	// Stop the server.
	srv1.Stop()

	// Restart — the key should be loaded from disk.
	srv2, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
		ServerSANs: []string{"127.0.0.1"},
	})
	s.Require().NoError(err)
	defer srv2.Stop()

	target2, _ := localserver.DiscoverTarget(stateDir)

	// Connect with the enrolled key — should work after restart.
	enrolledClient, err := bridgeclient.New(
		bridgeclient.WithTarget(target2),
		bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: mat.CABundlePath,
			CertPath:     mat.LocalClientCert,
			KeyPath:      mat.LocalClientKey,
			ServerName:   "server",
		}),
		bridgeclient.WithJWT(bridgeclient.JWTConfig{
			PrivateKeyPath: privPath,
			Issuer:         "local-client",
			Audience:       "bridge",
		}),
		bridgeclient.WithTimeout(5*time.Second),
	)
	s.Require().NoError(err)
	defer func() { _ = enrolledClient.Close() }()

	enrolledClient.SetProject("test")
	_, err = enrolledClient.ListSessions(ctx, &bridgev1.ListSessionsRequest{
		ProjectId: "test",
	})
	s.Require().NoError(err, "enrolled key should survive server restart")
}

// --- v1.1 Security Refactor E2E Tests ---

// TestTLSOnlyMode verifies that a server started with --mode tls accepts
// connections without a client certificate (JWT is the sole auth mechanism).
func (s *CLISuite) TestTLSOnlyMode() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	srv, err := localserver.Start(localserver.Config{
		StateDir:     stateDir,
		ListenAddr:   "127.0.0.1:0",
		SecurityMode: localserver.ModeTLS,
	})
	s.Require().NoError(err, "TLS-only server should start")
	defer srv.Stop()

	mode := localserver.DiscoverMode(stateDir)
	s.Assert().Equal(localserver.ModeTLS, mode, "mode file should say tls")

	// The server should be discoverable.
	target, discoveredMode := localserver.DiscoverTarget(stateDir)
	s.Require().NotEmpty(target, "TLS server should be discoverable")
	s.Assert().Equal(localserver.ModeTLS, discoveredMode)
}

// TestSecurityModeFlag verifies the bridgectl server start --mode flag
// is recognized by the CLI binary.
func (s *CLISuite) TestSecurityModeFlag() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// --mode with an invalid value should fail.
	cmd := exec.Command(cliBinary, "server", "start", "--mode", "insecure", "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	s.Assert().Error(err, "invalid --mode should fail")
	s.Assert().Contains(string(out), "unknown security mode", "error should mention the invalid mode")
}

// TestIdentityShowLocalMode verifies bridgectl identity show works after
// a secure server has generated PKI material.
func (s *CLISuite) TestIdentityShowLocalMode() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// Start a secure server so PKI is generated.
	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
	})
	s.Require().NoError(err)
	defer srv.Stop()

	cmd := exec.Command(cliBinary, "identity", "show")
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	s.Require().NoError(err, "identity show should succeed; output: %s", out)

	output := string(out)
	s.Assert().Contains(output, "Identity:")
	s.Assert().Contains(output, "Provider:")
	s.Assert().Contains(output, "Expires:")
	s.Assert().Contains(output, "Status:")
}

// TestIdentityShowNoServer verifies identity show fails gracefully when
// no PKI exists.
func (s *CLISuite) TestIdentityShowNoServer() {
	stateDir := s.testStateDir()

	cmd := exec.Command(cliBinary, "identity", "show")
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	_, err := cmd.CombinedOutput()
	s.Assert().Error(err, "identity show without PKI should fail")
}

// TestEnrollmentCreateAndList verifies the enrollment token lifecycle:
// create a token, verify it appears in the store.
func (s *CLISuite) TestEnrollmentCreateAndList() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// Create an enrollment token via CLI.
	cmd := exec.Command(cliBinary, "enrollment", "create",
		"--identity", "test-agent",
		"--expires", "5m")
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	s.Require().NoError(err, "enrollment create should succeed; output: %s", out)

	output := string(out)
	s.Assert().Contains(output, "brg_enroll_", "output should contain the token")
	s.Assert().Contains(output, "test-agent", "output should mention the identity")

	// Verify the token store file was created.
	storePath := filepath.Join(stateDir, "enrollment-tokens.json")
	_, err = os.Stat(storePath)
	s.Require().NoError(err, "enrollment store file should exist")

	data, err := os.ReadFile(storePath)
	s.Require().NoError(err)
	s.Assert().Contains(string(data), "test-agent")
	s.Assert().Contains(string(data), "brg_enroll_")
}

// TestClientSetupBundle verifies that client setup --bundle extracts
// credentials into the correct directory.
func (s *CLISuite) TestClientSetupBundle() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// Start a secure server to generate PKI, then issue a client cert.
	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
	})
	s.Require().NoError(err)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, _, issueErr := localserver.IssueClientCert(stateDir, "test-remote", logger)
	s.Require().NoError(issueErr)

	bundlePath := filepath.Join(localserver.CertsDir(stateDir), "clients", "test-remote", "test-remote-creds.tar.gz")
	bundleErr := localserver.BundleClientCreds(bundlePath, stateDir, "test-remote")
	s.Require().NoError(bundleErr)

	srv.Stop()

	// Set up a separate "client" state dir and run client setup --bundle.
	clientStateDir := s.T().TempDir()
	cmd := exec.Command(cliBinary, "client", "setup", "--bundle", bundlePath)
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+clientStateDir)
	out, err := cmd.CombinedOutput()
	s.Require().NoError(err, "client setup should succeed; output: %s", out)

	// Verify files were extracted.
	clientCertsDir := filepath.Join(clientStateDir, "certs")
	_, err = os.Stat(filepath.Join(clientCertsDir, "ca-bundle.crt"))
	s.Assert().NoError(err, "ca-bundle.crt should exist")
	_, err = os.Stat(filepath.Join(clientCertsDir, "test-remote.crt"))
	s.Assert().NoError(err, "test-remote.crt should exist")
	_, err = os.Stat(filepath.Join(clientCertsDir, "test-remote.key"))
	s.Assert().NoError(err, "test-remote.key should exist")
	_, err = os.Stat(filepath.Join(clientCertsDir, "jwt-signing.key"))
	s.Assert().NoError(err, "jwt-signing.key should exist")
}

// TestClientEnrollAutoDiscovery verifies that client enroll auto-discovers
// credentials from ~/.config/bridgectl/certs/ after client setup.
func (s *CLISuite) TestClientEnrollAutoDiscovery() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	serverStateDir := s.testStateDir()

	// Start a secure server.
	srv, err := localserver.Start(localserver.Config{
		StateDir:   serverStateDir,
		ListenAddr: "127.0.0.1:0",
	})
	s.Require().NoError(err)
	defer srv.Stop()

	target := srv.Addr()
	serverName := localserver.DiscoverServerName(serverStateDir)

	// Issue a client cert.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, _, issueErr := localserver.IssueClientCert(serverStateDir, "auto-disc-client", logger)
	s.Require().NoError(issueErr)

	bundlePath := filepath.Join(localserver.CertsDir(serverStateDir), "clients", "auto-disc-client", "auto-disc-client-creds.tar.gz")
	bundleErr := localserver.BundleClientCreds(bundlePath, serverStateDir, "auto-disc-client")
	s.Require().NoError(bundleErr)

	// Set up a separate "client" state dir with the bundle.
	clientStateDir := s.T().TempDir()
	setupCmd := exec.Command(cliBinary, "client", "setup", "--bundle", bundlePath)
	setupCmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+clientStateDir)
	setupOut, setupErr := setupCmd.CombinedOutput()
	s.Require().NoError(setupErr, "client setup should succeed; output: %s", setupOut)

	// Enroll using auto-discovery (no --ca, --cert, --key flags).
	enrollCmd := exec.Command(cliBinary, "client", "enroll",
		"--target", target,
		"--server-name", serverName)
	enrollCmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+clientStateDir)
	enrollOut, enrollErr := enrollCmd.CombinedOutput()
	s.Require().NoError(enrollErr, "client enroll should succeed with auto-discovery; output: %s", enrollOut)

	s.Assert().Contains(string(enrollOut), "Enrolled:", "should confirm enrollment")
}

// TestIdentityRenew verifies that identity renew re-issues the server
// certificate with a new expiry.
func (s *CLISuite) TestIdentityRenew() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// Start and immediately stop a secure server to generate PKI.
	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ListenAddr: "127.0.0.1:0",
	})
	s.Require().NoError(err)
	srv.Stop()

	// Record the original cert expiry.
	mat := localserver.LoadPKIMaterial(stateDir)
	origCert, err := pki.LoadCert(mat.ServerCertPath)
	s.Require().NoError(err)
	origExpiry := origCert.NotAfter

	// Run identity renew.
	cmd := exec.Command(cliBinary, "identity", "renew")
	cmd.Env = append(os.Environ(), "BRIDGECTL_STATE_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	s.Require().NoError(err, "identity renew should succeed; output: %s", out)
	s.Assert().Contains(string(out), "Expires:", "should show new expiry")

	// Verify the cert was renewed (new expiry >= original).
	renewedCert, err := pki.LoadCert(mat.ServerCertPath)
	s.Require().NoError(err)
	s.Assert().True(!renewedCert.NotAfter.Before(origExpiry),
		"renewed cert expiry %v should not be before original %v", renewedCert.NotAfter, origExpiry)
}

// TestSecurityConfigFromYAML verifies that a YAML config with the new
// security: block is correctly parsed and drives server startup.
func (s *CLISuite) TestSecurityConfigFromYAML() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}

	stateDir := s.testStateDir()

	// Write a YAML config with the new security block.
	configPath := filepath.Join(stateDir, "bridge.yaml")
	configContent := `
server:
  listen: "127.0.0.1:0"
security:
  transport:
    mode: mtls
  certificates:
    provider: auto
  authorization:
    mode: jwt
auth:
  jwt_max_ttl: "5m"
providers:
  echo:
    binary: "cat"
sessions:
  idle_timeout: "30m"
  stop_grace_period: "10s"
  subscriber_ttl: "30m"
`
	s.Require().NoError(os.WriteFile(configPath, []byte(configContent), 0o644))

	srv, err := localserver.Start(localserver.Config{
		StateDir:   stateDir,
		ConfigPath: configPath,
	})
	s.Require().NoError(err, "server with security config should start")
	defer srv.Stop()

	mode := localserver.DiscoverMode(stateDir)
	s.Assert().Equal(localserver.ModeSecure, mode, "mtls mode should be discoverable as secure")
}
