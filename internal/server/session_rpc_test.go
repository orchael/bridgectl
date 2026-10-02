package server

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/auth"
	"github.com/orchael/bridgectl/internal/bridge"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSessionIDFormatValidationRPC verifies that StartSession, StopSession,
// GetSession, WriteInput, and ResizeSession all reject non-UUID session_id
// with INVALID_ARGUMENT.
// Issue #153: session ID format validation.
func TestSessionIDFormatValidationRPC(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "cat", version: "1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  100,
		GlobalBurst:                100,
		StartSessionPerClientRPS:   100,
		StartSessionPerClientBurst: 100,
		SendInputPerSessionRPS:     100,
		SendInputPerSessionBurst:   100,
	}, "test", nil, nil, "", "")

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})

	badIDs := []string{
		"not-a-uuid",
		"12345",
		"",
		"zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz",
	}

	for _, badID := range badIDs {
		t.Run("StartSession/"+badID, func(t *testing.T) {
			_, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
				ProjectId: "proj",
				SessionId: badID,
				RepoPath:  t.TempDir(),
				Provider:  "cat",
			})
			if badID == "" {
				// Empty string is caught by validateStringField first.
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("code=%v want InvalidArgument", status.Code(err))
				}
			} else {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("StartSession(%q) code=%v want InvalidArgument", badID, status.Code(err))
				}
			}
		})

		if badID == "" {
			continue // Skip empty string for these RPCs (caught by required check).
		}

		t.Run("StopSession/"+badID, func(t *testing.T) {
			_, err := s.StopSession(ctx, &bridgev1.StopSessionRequest{SessionId: badID})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("StopSession(%q) code=%v want InvalidArgument", badID, status.Code(err))
			}
		})

		t.Run("GetSession/"+badID, func(t *testing.T) {
			_, err := s.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: badID})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("GetSession(%q) code=%v want InvalidArgument", badID, status.Code(err))
			}
		})

		t.Run("WriteInput/"+badID, func(t *testing.T) {
			_, err := s.WriteInput(ctx, &bridgev1.WriteInputRequest{
				SessionId: badID,
				ClientId:  "client-a",
				Data:      []byte("test"),
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("WriteInput(%q) code=%v want InvalidArgument", badID, status.Code(err))
			}
		})

		t.Run("ResizeSession/"+badID, func(t *testing.T) {
			_, err := s.ResizeSession(ctx, &bridgev1.ResizeSessionRequest{
				SessionId: badID,
				ClientId:  "client-a",
				Cols:      80,
				Rows:      24,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("ResizeSession(%q) code=%v want InvalidArgument", badID, status.Code(err))
			}
		})
	}
}

// TestGlobalRateLimitExhausted verifies that the global rate limiter
// returns ResourceExhausted when the limit is exceeded.
// Issue #153: rate limiting.
func TestGlobalRateLimitExhausted(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "cat", version: "1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	// Very tight rate limit: 1 RPS, burst 1.
	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  1,
		GlobalBurst:                1,
		StartSessionPerClientRPS:   100,
		StartSessionPerClientBurst: 100,
		SendInputPerSessionRPS:     100,
		SendInputPerSessionBurst:   100,
	}, "test", nil, nil, "", "")

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})

	// First call should succeed (consumes the burst token).
	_, err := s.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: uuid.NewString()})
	// The first call may return NotFound (session does not exist) but not
	// ResourceExhausted.
	if status.Code(err) == codes.ResourceExhausted {
		t.Fatal("first call was rate limited, expected it to pass")
	}

	// Rapid subsequent calls should eventually be rate limited.
	var rateLimited bool
	for i := 0; i < 50; i++ {
		_, err := s.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: uuid.NewString()})
		if status.Code(err) == codes.ResourceExhausted {
			rateLimited = true
			break
		}
	}
	if !rateLimited {
		t.Fatal("expected at least one ResourceExhausted response after burst")
	}
}

// TestListSessionsRPC verifies the ListSessions RPC returns sessions filtered
// by project.
// Issue #153: ListSessions.
func TestListSessionsRPC(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "cat", version: "1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  100,
		GlobalBurst:                100,
		StartSessionPerClientRPS:   100,
		StartSessionPerClientBurst: 100,
		SendInputPerSessionRPS:     100,
		SendInputPerSessionBurst:   100,
	}, "test", nil, nil, "", "")

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj-list"})

	// Start two sessions.
	sid1 := uuid.NewString()
	sid2 := uuid.NewString()
	for _, sid := range []string{sid1, sid2} {
		if _, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
			ProjectId: "proj-list",
			SessionId: sid,
			RepoPath:  t.TempDir(),
			Provider:  "cat",
		}); err != nil {
			t.Fatalf("StartSession %s: %v", sid, err)
		}
	}

	listResp, err := s.ListSessions(ctx, &bridgev1.ListSessionsRequest{ProjectId: "proj-list"})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(listResp.GetSessions()) != 2 {
		t.Fatalf("ListSessions returned %d sessions, want 2", len(listResp.GetSessions()))
	}

	// Verify both session IDs are present.
	ids := map[string]bool{}
	for _, sess := range listResp.GetSessions() {
		ids[sess.GetSessionId()] = true
	}
	if !ids[sid1] || !ids[sid2] {
		t.Fatalf("expected both session IDs in response, got: %v", ids)
	}

	// Clean up.
	for _, sid := range []string{sid1, sid2} {
		_, _ = s.StopSession(ctx, &bridgev1.StopSessionRequest{SessionId: sid, Force: true})
	}
}

// TestWriteInputRateLimitExhausted verifies that the per-session write rate
// limiter returns ResourceExhausted when the limit is exceeded.
// Issue #153: rate limiting.
func TestWriteInputRateLimitExhausted(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "cat", version: "1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	supervisor := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024, time.Minute)
	defer supervisor.Close()

	// Very tight write rate limit: 1 RPS, burst 2.
	s := New(supervisor, registry, nil, RateLimitConfig{
		GlobalRPS:                  1000,
		GlobalBurst:                1000,
		StartSessionPerClientRPS:   1000,
		StartSessionPerClientBurst: 1000,
		SendInputPerSessionRPS:     1,
		SendInputPerSessionBurst:   2,
	}, "test", nil, nil, "", "")

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})
	sid := uuid.NewString()

	if _, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId:   "proj",
		SessionId:   sid,
		RepoPath:    t.TempDir(),
		Provider:    "cat",
		InitialCols: 80,
		InitialRows: 24,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Attach the client as writer via the supervisor.
	if _, err := supervisor.Attach(sid, "cli", 0, bridge.AttachRoleWriter); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Send enough writes to exhaust the burst.
	var rateLimited bool
	for i := 0; i < 50; i++ {
		_, err := s.WriteInput(ctx, &bridgev1.WriteInputRequest{
			SessionId: sid,
			ClientId:  "cli",
			Data:      []byte("x"),
		})
		if status.Code(err) == codes.ResourceExhausted {
			rateLimited = true
			break
		}
	}
	if !rateLimited {
		t.Fatal("expected ResourceExhausted for write input after burst")
	}

	_ = supervisor.Stop(sid, true)
}
