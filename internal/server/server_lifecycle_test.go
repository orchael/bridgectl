package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/auth"
	"github.com/orchael/bridgectl/internal/bridge"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type attachStream struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	events []*bridgev1.AttachSessionEvent
}

func newAttachStream(ctx context.Context) *attachStream {
	streamCtx, cancel := context.WithCancel(ctx)
	return &attachStream{ctx: streamCtx, cancel: cancel}
}

func (s *attachStream) SetHeader(metadata.MD) error  { return nil }
func (s *attachStream) SendHeader(metadata.MD) error { return nil }
func (s *attachStream) SetTrailer(metadata.MD)       {}
func (s *attachStream) Context() context.Context     { return s.ctx }
func (s *attachStream) SendMsg(any) error            { return nil }
func (s *attachStream) RecvMsg(any) error            { return nil }
func (s *attachStream) Send(ev *bridgev1.AttachSessionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *attachStream) snapshot() []*bridgev1.AttachSessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*bridgev1.AttachSessionEvent, len(s.events))
	copy(out, s.events)
	return out
}

func TestBridgeServerSessionLifecycle(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "cat", version: "1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  10,
		GlobalBurst:                10,
		StartSessionPerClientRPS:   10,
		StartSessionPerClientBurst: 10,
		SendInputPerSessionRPS:     10,
		SendInputPerSessionBurst:   10,
	}, "test-instance", nil, nil, "")

	sessionID := uuid.NewString()
	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "project-a"})

	startResp, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "project-a",
		SessionId:   sessionID,
		RepoPath:    t.TempDir(),
		Provider:    "cat",
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if startResp.GetSessionId() != sessionID {
		t.Fatalf("StartSession resp=%+v", startResp)
	}

	getResp, err := s.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: sessionID})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if getResp.GetSessionId() != sessionID {
		t.Fatalf("GetSession resp=%+v", getResp)
	}

	listResp, err := s.ListSessions(ctx, &bridgev1.ListSessionsRequest{ProjectId: "project-a"})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(listResp.GetSessions()) != 1 {
		t.Fatalf("ListSessions len=%d want 1", len(listResp.GetSessions()))
	}

	stream := newAttachStream(ctx)
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- s.AttachSession(&bridgev1.AttachSessionRequest{
			SessionId: sessionID,
			ClientId:  "client-a",
		}, stream)
	}()

	waitForAttachEvent(t, stream, bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED)

	writeResp, err := s.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sessionID,
		ClientId:  "client-a",
		Data:      []byte("hello\n"),
	})
	if err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	if !writeResp.GetAccepted() {
		t.Fatalf("WriteInput resp=%+v", writeResp)
	}

	if err := waitForAttachOutput(stream, "hello"); err != nil {
		t.Fatal(err)
	}

	resizeResp, err := s.ResizeSession(ctx, &bridgev1.ResizeSessionRequest{
		SessionId: sessionID,
		ClientId:  "client-a",
		Cols:      100,
		Rows:      40,
	})
	if err != nil {
		t.Fatalf("ResizeSession: %v", err)
	}
	if !resizeResp.GetApplied() {
		t.Fatalf("ResizeSession resp=%+v", resizeResp)
	}

	stream.cancel()
	if err := <-attachDone; err != nil {
		t.Fatalf("AttachSession: %v", err)
	}

	stopResp, err := s.StopSession(ctx, &bridgev1.StopSessionRequest{SessionId: sessionID, Force: true})
	if err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if stopResp.GetStatus() != bridgev1.SessionStatus_SESSION_STATUS_STOPPING {
		t.Fatalf("StopSession resp=%+v", stopResp)
	}
}

func TestBridgeServerValidationAndPermissions(t *testing.T) {
	registry := bridge.NewRegistry()
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:   10,
		GlobalBurst: 10,
	}, "test-instance", nil, nil, "")
	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "project-a"})

	if _, err := s.ListSessions(ctx, &bridgev1.ListSessionsRequest{ProjectId: "project-b"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ListSessions code=%v want %v", status.Code(err), codes.PermissionDenied)
	}
	if _, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("StartSession code=%v want %v", status.Code(err), codes.InvalidArgument)
	}
	if _, err := s.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: uuid.NewString()}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetSession code=%v want %v", status.Code(err), codes.NotFound)
	}
	if _, err := s.Health(context.Background(), &bridgev1.HealthRequest{}); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestBridgeServerStartSessionDirAccess(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "cat", version: "1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  10,
		GlobalBurst:                10,
		StartSessionPerClientRPS:   10,
		StartSessionPerClientBurst: 10,
	}, "test-instance", nil, nil, "")

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "project-a"})

	t.Run("nonexistent directory", func(t *testing.T) {
		_, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
			ProjectId: "project-a",
			SessionId: uuid.NewString(),
			RepoPath:  filepath.Join(t.TempDir(), "does-not-exist"),
			Provider:  "cat",
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("got code %v, want PermissionDenied", status.Code(err))
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "file-*")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		_, err = s.StartSession(ctx, &bridgev1.StartSessionRequest{
			ProjectId: "project-a",
			SessionId: uuid.NewString(),
			RepoPath:  f.Name(),
			Provider:  "cat",
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("got code %v, want PermissionDenied", status.Code(err))
		}
	})

	t.Run("read-only directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		_, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
			ProjectId: "project-a",
			SessionId: uuid.NewString(),
			RepoPath:  dir,
			Provider:  "cat",
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("got code %v, want PermissionDenied", status.Code(err))
		}
	})
}

func TestBridgeServerStartSessionUsesConfiguredFallbacks(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "primary", healthErr: context.DeadlineExceeded}); err != nil {
		t.Fatalf("Register primary: %v", err)
	}
	if err := registry.Register(&serverTestProvider{id: "secondary", version: "1"}); err != nil {
		t.Fatalf("Register secondary: %v", err)
	}

	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  10,
		GlobalBurst:                10,
		StartSessionPerClientRPS:   10,
		StartSessionPerClientBurst: 10,
		SendInputPerSessionRPS:     10,
		SendInputPerSessionBurst:   10,
	}, "test-instance", map[string][]string{
		"primary": {"secondary"},
	}, nil, "")

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "project-a"})
	sessionID := uuid.NewString()
	resp, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId: "project-a",
		SessionId: sessionID,
		RepoPath:  t.TempDir(),
		Provider:  "primary",
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if resp.GetSessionId() != sessionID {
		t.Fatalf("StartSession resp=%+v", resp)
	}

	info, err := supervisor.Get(sessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Provider != "secondary" {
		t.Fatalf("Provider=%q want secondary", info.Provider)
	}

	if err := supervisor.Stop(sessionID, true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestBridgeServerStartSessionNoFallbackWhenDisabled verifies that a failing
// primary provider is NOT rescued by a fallback when providerFallbacks is nil
// (i.e. the feature_flags.provider_fallbacks flag is false).
func TestBridgeServerStartSessionNoFallbackWhenDisabled(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "primary", healthErr: context.DeadlineExceeded}); err != nil {
		t.Fatalf("Register primary: %v", err)
	}
	if err := registry.Register(&serverTestProvider{id: "secondary", version: "1"}); err != nil {
		t.Fatalf("Register secondary: %v", err)
	}

	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	// providerFallbacks is nil — simulating feature flag disabled.
	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  10,
		GlobalBurst:                10,
		StartSessionPerClientRPS:   10,
		StartSessionPerClientBurst: 10,
		SendInputPerSessionRPS:     10,
		SendInputPerSessionBurst:   10,
	}, "test-instance", nil, nil, "")

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "project-a"})
	_, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId: "project-a",
		SessionId: uuid.NewString(),
		RepoPath:  t.TempDir(),
		Provider:  "primary",
	})
	if err == nil {
		t.Fatal("expected StartSession to fail when primary is down and fallbacks are disabled")
	}
}

func TestAttachSessionSendsExitEvent(t *testing.T) {
	registry := bridge.NewRegistry()
	// The default (non-cat) serverTestProvider runs trueBin which exits
	// immediately with code 0, simulating an agent that exits on its own.
	if err := registry.Register(&serverTestProvider{id: "short", version: "1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  10,
		GlobalBurst:                10,
		StartSessionPerClientRPS:   10,
		StartSessionPerClientBurst: 10,
		SendInputPerSessionRPS:     10,
		SendInputPerSessionBurst:   10,
	}, "test-instance", nil, nil, "")

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "project-a"})
	sessionID := uuid.NewString()

	_, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "project-a",
		SessionId:   sessionID,
		RepoPath:    t.TempDir(),
		Provider:    "short",
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	stream := newAttachStream(ctx)
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- s.AttachSession(&bridgev1.AttachSessionRequest{
			SessionId: sessionID,
			ClientId:  "client-exit",
		}, stream)
	}()

	// The process exits immediately; AttachSession should send a
	// SESSION_EXIT event and then return.
	select {
	case err := <-attachDone:
		if err != nil {
			t.Fatalf("AttachSession returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AttachSession did not return after process exit")
	}

	var found bool
	for _, ev := range stream.snapshot() {
		if ev.GetType() == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_SESSION_EXIT {
			found = true
			if !ev.GetExitRecorded() {
				t.Errorf("SESSION_EXIT event: exit_recorded=false, want true")
			}
			if ev.GetExitCode() != 0 {
				t.Errorf("SESSION_EXIT event: exit_code=%d, want 0", ev.GetExitCode())
			}
		}
	}
	if !found {
		t.Fatalf("no SESSION_EXIT event received; events: %v", stream.snapshot())
	}
}

func waitForAttachEvent(t *testing.T, stream *attachStream, typ bridgev1.AttachEventType) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range stream.snapshot() {
			if ev.GetType() == typ {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for attach event %v", typ)
}

func waitForAttachOutput(stream *attachStream, needle string) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range stream.snapshot() {
			if ev.GetType() == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT && bytes.Contains(ev.GetPayload(), []byte(needle)) {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return status.Error(codes.DeadlineExceeded, "timed out waiting for attach output")
}
