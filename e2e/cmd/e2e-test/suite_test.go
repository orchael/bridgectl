//go:build e2e

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/telemetry"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

// Flag variables for the test binary.
// Run with: ./e2e-suite -test.v -test.timeout 300s -bridge.target bridge:9445 ...
var (
	suiteTarget  = flag.String("bridge.target", "bridge:9445", "bridge address")
	suiteCACert  = flag.String("bridge.cacert", "", "CA bundle path")
	suiteCert    = flag.String("bridge.cert", "", "client cert path")
	suiteKey     = flag.String("bridge.key", "", "client key path")
	suiteJWTKey  = flag.String("bridge.jwt-key", "", "JWT signing key path")
	suiteIssuer  = flag.String("bridge.jwt-issuer", "e2e", "JWT issuer")
	suiteRepo    = flag.String("bridge.repo", "/tmp/bridgectl", "repo path")
	suiteEvents  = flag.String("bridge.telemetry-events", "/telemetry/segments", "telemetry segmented JSONL directory")
	suiteTimeout = flag.Duration("bridge.timeout", 15*time.Minute, "per-scenario timeout")
)

// TestMain ensures custom flags are parsed before running the suite.
func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}

// BridgeSuite tests end-to-end provider scenarios against a live bridge daemon.
type BridgeSuite struct {
	suite.Suite
	client *bridgeclient.Client
}

func (s *BridgeSuite) SetupSuite() {
	client, err := bridgeclient.New(
		bridgeclient.WithTarget(*suiteTarget),
		bridgeclient.WithTimeout(*suiteTimeout),
		bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
			CABundlePath: *suiteCACert,
			CertPath:     *suiteCert,
			KeyPath:      *suiteKey,
			ServerName:   "bridge",
		}),
		bridgeclient.WithJWT(bridgeclient.JWTConfig{
			PrivateKeyPath: *suiteJWTKey,
			Issuer:         *suiteIssuer,
			Audience:       "bridge",
		}),
	)
	s.Require().NoError(err, "connect to bridge")
	client.SetProject("e2e")
	s.client = client
}

func (s *BridgeSuite) TearDownSuite() {
	if s.client != nil {
		_ = s.client.Close()
	}
}

func (s *BridgeSuite) TestClaude() {
	s.runProviderScenario(s.providerScenario("claude"))
}

func (s *BridgeSuite) TestOpencode() {
	s.runProviderScenario(s.providerScenario("opencode"))
}

func (s *BridgeSuite) TestGemini() {
	s.runProviderScenario(s.providerScenario("codex"))
}

// TestEcho validates bridge session lifecycle using the no-auth echo provider (cat).
// It runs unconditionally and proves the bridge is reachable and sessions work.
func (s *BridgeSuite) TestEcho() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessionID := uuid.NewString()
	_, err := s.client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "e2e",
		SessionId:   sessionID,
		RepoPath:    *suiteRepo,
		Provider:    "echo",
		InitialCols: 120,
		InitialRows: 40,
	})
	s.Require().NoError(err, "start echo session")

	stream, err := s.client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  uuid.NewString(),
	})
	s.Require().NoError(err, "attach echo session")

	var log transcript
	done := make(chan error, 1)
	go func() {
		done <- stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT {
				log.append(ev.Payload)
			}
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR {
				return errors.New(ev.Error)
			}
			return nil
		})
	}()

	marker := "ECHO_BRIDGE_OK"
	_, err = s.client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  stream.ClientID(),
		Data:      []byte(marker + "\n"),
	})
	s.Require().NoError(err, "write echo input")

	err = waitForLiteral(&log, marker, 10*time.Second)
	s.Require().NoError(err, "echo marker not received")

	_, _ = s.client.StopSession(context.Background(), &bridgev1.StopSessionRequest{
		SessionId: sessionID,
		Force:     true,
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// TestTelemetry proves that the Docker bridge observes a real provider's
// output and authorized client input, then persists their correlated events.
func (s *BridgeSuite) TestTelemetry() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessionID := uuid.NewString()
	_, err := s.client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "e2e",
		SessionId:   sessionID,
		RepoPath:    *suiteRepo,
		Provider:    "telemetry-fixture",
		InitialCols: 120,
		InitialRows: 40,
	})
	s.Require().NoError(err, "start telemetry fixture session")

	stream, err := s.client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  uuid.NewString(),
	})
	s.Require().NoError(err, "attach telemetry fixture session")

	var log transcript
	done := make(chan error, 1)
	go func() {
		done <- stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT {
				log.append(ev.Payload)
			}
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR {
				return errors.New(ev.Error)
			}
			return nil
		})
	}()

	s.Require().NoError(waitForLiteral(&log, "Do you want me to run this telemetry fixture?", 10*time.Second), "fixture question not received")
	_, err = s.client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  stream.ClientID(),
		Data:      []byte("yes\n"),
	})
	s.Require().NoError(err, "answer telemetry fixture")
	s.Require().NoError(waitForLiteral(&log, "TELEMETRY_FIXTURE_ANSWER=yes", 10*time.Second), "fixture answer not received")

	_, err = s.client.StopSession(ctx, &bridgev1.StopSessionRequest{SessionId: sessionID, Force: true})
	s.Require().NoError(err, "stop telemetry fixture session")
	time.Sleep(250 * time.Millisecond)
	events, err := waitForTelemetryEvents(*suiteEvents, sessionID, 10*time.Second)
	s.Require().NoError(err, "persist correlated telemetry events")
	for _, event := range events {
		s.T().Logf("telemetry event: kind=%s stream=%s sequence=%d class=%s decision=%s fingerprint=%s bytes=%d redactions=%d", event.Kind, event.Stream, event.Sequence, event.Class, event.Decision, event.Fingerprint, event.ByteCount, event.Redactions)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (s *BridgeSuite) TestUnprotectedCodexFilesystemBehavior() {
	s.runUnprotectedFilesystemScenario(s.providerScenario("codex"))
}

func (s *BridgeSuite) TestUnprotectedClaudeFilesystemBehavior() {
	s.runUnprotectedFilesystemScenario(s.providerScenario("claude"))
}

func (s *BridgeSuite) providerScenario(name string) providerScenario {
	for _, scenario := range scenarios {
		if scenario.name == name {
			return scenario
		}
	}
	s.T().Fatalf("provider scenario %q not found", name)
	return providerScenario{}
}

func (s *BridgeSuite) runProviderScenario(scenario providerScenario) {
	if os.Getenv(scenario.requiredEnv) == "" {
		s.T().Skipf("skipping %s: %s not set", scenario.name, scenario.requiredEnv)
	}
	err := s.executeScenario(scenario)
	s.Require().NoError(err, "provider scenario %s", scenario.name)
}

func (s *BridgeSuite) runUnprotectedFilesystemScenario(scenario providerScenario) {
	expectation := strings.TrimSpace(os.Getenv("E2E_UNPROTECTED_EXPECT"))
	if expectation == "" {
		s.T().Skip("skipping manual unprotected-mode e2e: E2E_UNPROTECTED_EXPECT not set")
	}
	if expectation != "protected" && expectation != "enabled" {
		s.T().Fatalf("E2E_UNPROTECTED_EXPECT=%q, want protected or enabled", expectation)
	}
	if os.Getenv(scenario.requiredEnv) == "" {
		s.T().Skipf("skipping %s: %s not set", scenario.name, scenario.requiredEnv)
	}

	err := s.executeUnprotectedFilesystemScenario(scenario, expectation)
	s.Require().NoError(err, "%s unprotected filesystem scenario", scenario.name)
}

func (s *BridgeSuite) executeUnprotectedFilesystemScenario(scenario providerScenario, expectation string) error {
	ctx, cancel := context.WithTimeout(context.Background(), *suiteTimeout)
	defer cancel()

	id := uuid.NewString()
	markerName := fmt.Sprintf("bridge-unprotected-%s-%s.txt", scenario.name, id)
	markerRelPath := filepath.Join(".git", markerName)
	markerAbsPath := filepath.Join(*suiteRepo, markerRelPath)
	markerContent := fmt.Sprintf("BRIDGE_UNPROTECTED_%s_OK_%s", strings.ToUpper(scenario.name), id)
	doneMarker := fmt.Sprintf("BRIDGE_DONE_%s", id)
	_ = os.Remove(markerAbsPath)
	defer func() { _ = os.Remove(markerAbsPath) }()

	sessionID := uuid.NewString()
	_, err := s.client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "e2e",
		SessionId:   sessionID,
		RepoPath:    *suiteRepo,
		Provider:    scenario.name,
		InitialCols: 120,
		InitialRows: 40,
	})
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer func() {
		_, _ = s.client.StopSession(context.Background(), &bridgev1.StopSessionRequest{
			SessionId: sessionID,
			Force:     true,
		})
	}()

	stream, err := s.client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  uuid.NewString(),
	})
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}

	var log transcript
	done := make(chan error, 1)
	go func() {
		done <- stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT {
				log.append(ev.Payload)
			}
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR {
				return errors.New(ev.Error)
			}
			return nil
		})
	}()

	if err := waitForMatch(&log, scenario.promptRe, scenario.startTimeout); err != nil {
		return fmt.Errorf("initial prompt: %w\ntranscript:\n%s", err, log.snapshot())
	}
	if expectation == "enabled" && scenario.name == "claude" && strings.Contains(log.snapshot(), "Bypass Permissions mode") {
		if _, err := s.client.WriteInput(ctx, &bridgev1.WriteInputRequest{
			SessionId: sessionID,
			ClientId:  stream.ClientID(),
			Data:      []byte("\x1bOB\r"),
		}); err != nil {
			return fmt.Errorf("accept claude bypass prompt: %w", err)
		}
		time.Sleep(2 * time.Second)
	}

	prompt := fmt.Sprintf("This is an automated bridge e2e test. Use your file editing or shell tool to create the file %s with exactly this content: %s. Do not create any other files. Do not ask for confirmation. After the file has been written, reply with exactly %s and nothing else.", markerRelPath, markerContent, doneMarker)

	input := []byte(prompt + "\r")
	if expectation == "enabled" && scenario.name == "claude" {
		input = []byte(prompt)
	}
	if _, err := s.client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  stream.ClientID(),
		Data:      input,
	}); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	if expectation == "enabled" && scenario.name == "claude" {
		time.Sleep(2 * time.Second)
		if _, err := s.client.WriteInput(ctx, &bridgev1.WriteInputRequest{
			SessionId: sessionID,
			ClientId:  stream.ClientID(),
			Data:      []byte("\r"),
		}); err != nil {
			return fmt.Errorf("submit claude prompt: %w", err)
		}
	}

	if expectation == "enabled" {
		if err := waitForFileContent(markerAbsPath, markerContent, 5*time.Minute); err != nil {
			return fmt.Errorf("unprotected marker file: %w\ntranscript:\n%s", err, log.snapshot())
		}
		if err := waitForLiteral(&log, doneMarker, 30*time.Second); err != nil {
			return fmt.Errorf("completion marker: %w\ntranscript:\n%s", err, log.snapshot())
		}
		return nil
	}

	if err := waitForFileContent(markerAbsPath, markerContent, 90*time.Second); err == nil {
		return fmt.Errorf("protected mode wrote marker file %q", markerAbsPath)
	}

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("stream ended before protected-mode assertion completed: %w\ntranscript:\n%s", err, log.snapshot())
		}
		return fmt.Errorf("stream ended before protected-mode assertion completed\ntranscript:\n%s", log.snapshot())
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// executeScenario runs a multi-turn provider conversation and asserts correctness.
func (s *BridgeSuite) executeScenario(scenario providerScenario) error {
	ctx, cancel := context.WithTimeout(context.Background(), *suiteTimeout)
	defer cancel()

	sessionID := uuid.NewString()
	_, err := s.client.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "e2e",
		SessionId:   sessionID,
		RepoPath:    *suiteRepo,
		Provider:    scenario.name,
		InitialCols: 120,
		InitialRows: 40,
	})
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}

	stream, err := s.client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
		SessionId: sessionID,
		ClientId:  uuid.NewString(),
	})
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}

	var log transcript
	done := make(chan error, 1)
	go func() {
		done <- stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT {
				log.append(ev.Payload)
			}
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR {
				return errors.New(ev.Error)
			}
			return nil
		})
	}()

	if err := waitForMatch(&log, scenario.promptRe, scenario.startTimeout); err != nil {
		return fmt.Errorf("initial prompt: %w\ntranscript:\n%s", err, log.snapshot())
	}

	turn1Marker := "BRIDGE_TURN_ONE_OK"
	if _, err := s.client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  stream.ClientID(),
		Data:      []byte("Reply with exactly " + turn1Marker + " and nothing else.\n"),
	}); err != nil {
		return fmt.Errorf("write turn 1: %w", err)
	}
	if err := waitForLiteral(&log, turn1Marker, scenario.turnTimeout); err != nil {
		return fmt.Errorf("turn 1 response: %w\ntranscript:\n%s", err, log.snapshot())
	}
	if err := waitForMatch(&log, scenario.promptRe, scenario.turnTimeout); err != nil {
		return fmt.Errorf("turn 1 prompt return: %w\ntranscript:\n%s", err, log.snapshot())
	}

	if _, err := s.client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  stream.ClientID(),
		Data:      []byte("Ask me exactly one short clarifying question, then wait for my answer.\n"),
	}); err != nil {
		return fmt.Errorf("write turn 2: %w", err)
	}
	if err := waitForMatch(&log, scenario.questionCheck, scenario.turnTimeout); err != nil {
		return fmt.Errorf("turn 2 question: %w\ntranscript:\n%s", err, log.snapshot())
	}
	if err := waitForMatch(&log, scenario.promptRe, scenario.turnTimeout); err != nil {
		return fmt.Errorf("turn 2 prompt return: %w\ntranscript:\n%s", err, log.snapshot())
	}

	turn3Marker := "BRIDGE_FOLLOWUP_OK"
	if _, err := s.client.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  stream.ClientID(),
		Data:      []byte("Blue. Reply with exactly " + turn3Marker + " and nothing else.\n"),
	}); err != nil {
		return fmt.Errorf("write turn 3: %w", err)
	}
	if err := waitForLiteral(&log, turn3Marker, scenario.turnTimeout); err != nil {
		return fmt.Errorf("turn 3 response: %w\ntranscript:\n%s", err, log.snapshot())
	}

	_, _ = s.client.StopSession(context.Background(), &bridgev1.StopSessionRequest{
		SessionId: sessionID,
		Force:     true,
	})
	cancel()
	select {
	case err := <-done:
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("stream: %w", err)
		}
	case <-time.After(5 * time.Second):
	}
	return nil
}

func waitForFileContent(path, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == want {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %q to contain %q", path, want)
}

func waitForTelemetryEvents(path, sessionID string, timeout time.Duration) ([]telemetry.Event, error) {
	deadline := time.Now().Add(timeout)
	lastErr := errors.New("no telemetry events observed")
	for time.Now().Before(deadline) {
		events, err := telemetry.ReadEvents(path)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		seen := make(map[telemetry.EventKind]telemetry.Event)
		var sessionEvents []telemetry.Event
		var providerText, userText strings.Builder
		var previousSequence uint64
		ordered := true
		for _, event := range events {
			if event.SourceID == "e2e-bridge" && event.SessionID == sessionID {
				seen[event.Kind] = event
				sessionEvents = append(sessionEvents, event)
				if previousSequence > 0 && event.Sequence <= previousSequence {
					ordered = false
				}
				previousSequence = event.Sequence
				if event.Kind == telemetry.EventProviderOutput {
					providerText.WriteString(event.Text)
				}
				if event.Kind == telemetry.EventUserInput {
					userText.WriteString(event.Text)
				}
			}
		}
		question, hasQuestion := seen[telemetry.EventQuestion]
		answer, hasAnswer := seen[telemetry.EventAnswer]
		_, hasProviderOutput := seen[telemetry.EventProviderOutput]
		_, hasUserInput := seen[telemetry.EventUserInput]
		_, hasStarted := seen[telemetry.EventSessionStarted]
		_, hasEnded := seen[telemetry.EventSessionEnded]
		if hasStarted && hasProviderOutput && hasUserInput && hasQuestion && hasAnswer && hasEnded && ordered &&
			question.Class == telemetry.ClassPermission &&
			answer.Decision == telemetry.DecisionAccepted &&
			answer.Fingerprint == question.Fingerprint &&
			strings.Contains(providerText.String(), "Do you want me to run this telemetry fixture?") &&
			strings.Contains(userText.String(), "yes") {
			return sessionEvents, nil
		}
		lastErr = fmt.Errorf("full capture unexpected: started=%t provider_output=%t user_input=%t question=%t answer=%t ended=%t ordered=%t", hasStarted, hasProviderOutput, hasUserInput, hasQuestion, hasAnswer, hasEnded, ordered)
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out reading telemetry for session %s from %s: %w", sessionID, path, lastErr)
}

// TestBridgeSuite is the entry point that runs all provider tests.
func TestBridgeSuite(t *testing.T) {
	suite.Run(t, new(BridgeSuite))
}
