package server

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/auth"
	"github.com/orchael/bridgectl/internal/bridge"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// trueBin is the absolute path to the "true" binary, resolved once via
// LookPath so tests work on both Linux (/bin/true) and macOS (/usr/bin/true).
var trueBin = func() string {
	if p, err := exec.LookPath("true"); err == nil {
		return p
	}
	return "/usr/bin/true"
}()

type serverTestProvider struct {
	id        string
	healthErr error
	version   string
}

func (p *serverTestProvider) ID() string                    { return p.id }
func (p *serverTestProvider) Binary() string                { return trueBin }
func (p *serverTestProvider) PromptPattern() *regexp.Regexp { return nil }
func (p *serverTestProvider) StartupTimeout() time.Duration { return time.Second }
func (p *serverTestProvider) StopGrace() time.Duration      { return time.Second }
func (p *serverTestProvider) BuildCommand(context.Context, bridge.SessionConfig) (*exec.Cmd, error) {
	if p.id == "cat" {
		return exec.Command("/bin/cat"), nil
	}
	return exec.Command(trueBin), nil
}
func (p *serverTestProvider) ValidateStartup(context.Context) error { return nil }
func (p *serverTestProvider) Health(context.Context) error          { return p.healthErr }
func (p *serverTestProvider) Version(context.Context) (string, error) {
	return p.version, nil
}

func TestValidationHelpers(t *testing.T) {
	if err := validateStringField("field", "ok", 10, false); err != nil {
		t.Fatalf("validateStringField: %v", err)
	}
	if err := validateStringField("field", "", 10, false); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty string code=%v want %v", status.Code(err), codes.InvalidArgument)
	}
	if err := validateOptionalStringField("field", "", 10, false); err != nil {
		t.Fatalf("validateOptionalStringField empty: %v", err)
	}
	if err := validateByteField("data", []byte("x"), 1); err != nil {
		t.Fatalf("validateByteField: %v", err)
	}
	if err := validateUUIDField("session_id", "not-a-uuid"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("validateUUIDField code=%v want %v", status.Code(err), codes.InvalidArgument)
	}
}

func TestRateLimiters(t *testing.T) {
	now := time.Now()
	bucket := newTokenBucket(1, 1, now)
	if !bucket.allow(now) {
		t.Fatal("first allow was false")
	}
	if bucket.allow(now) {
		t.Fatal("second allow was true without refill")
	}
	if !bucket.allow(now.Add(1100 * time.Millisecond)) {
		t.Fatal("allow after refill was false")
	}

	limiter := newKeyedLimiter(1, 1)
	limiter.ttl = time.Millisecond
	if !limiter.allow("client-a") {
		t.Fatal("keyed limiter first allow was false")
	}
	limiter.mu.Lock()
	limiter.buckets["client-a"].lastSeen = time.Now().Add(-time.Hour)
	limiter.cleanupLocked(time.Now())
	_, exists := limiter.buckets["client-a"]
	limiter.mu.Unlock()
	if exists {
		t.Fatal("stale bucket still existed after cleanup")
	}
}

func TestCircuitBreaker(t *testing.T) {
	now := time.Now()
	// Bucket with rate=1/s, burst=1; use an empty bucket so every call is a violation.
	bucket := newTokenBucket(1, 1, now)
	bucket.allow(now) // drain the one token

	// Drive consecutive violations up to the threshold.
	for i := 0; i < circuitBreakerThreshold-1; i++ {
		bucket.allow(now)
		if bucket.isBanned(now) {
			t.Fatalf("circuit breaker tripped too early at violation %d", i+1)
		}
	}
	// The next violation should trip the breaker.
	bucket.allow(now)
	if !bucket.isBanned(now) {
		t.Fatal("circuit breaker did not trip after threshold violations")
	}
	// Violations reset after the ban expires.
	afterBan := now.Add(circuitBreakerBanDuration + time.Second)
	if bucket.isBanned(afterBan) {
		t.Fatal("bucket still banned after ban duration elapsed")
	}
	// A successful allow resets the consecutive violation counter.
	bucket2 := newTokenBucket(10, 10, now)
	bucket2.allow(now) // succeeds, resets counter
	if bucket2.consecutiveViolations != 0 {
		t.Fatal("consecutive violations not reset after successful allow")
	}

	// keyedLimiter.banned() mirrors the bucket state.
	// Drive the underlying bucket directly with a fixed timestamp to avoid
	// token refills from wall-clock progression making the test flaky.
	limiter := newKeyedLimiter(1, 1)
	if limiter.banned("x") {
		t.Fatal("banned returned true for unknown key")
	}
	// Insert a pre-drained bucket so all subsequent allow calls are violations.
	limiter.mu.Lock()
	drained := newTokenBucket(1, 1, now)
	drained.allow(now) // consume the single token
	limiter.buckets["x"] = drained
	limiter.mu.Unlock()
	for i := 0; i < circuitBreakerThreshold; i++ {
		limiter.mu.Lock()
		limiter.buckets["x"].allow(now)
		limiter.mu.Unlock()
	}
	if !limiter.banned("x") {
		t.Fatal("limiter.banned returned false after threshold violations")
	}
}

func TestBridgeHelpersAndProviderResponses(t *testing.T) {
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "healthy", version: "v1.2.3"}); err != nil {
		t.Fatalf("Register healthy: %v", err)
	}
	if err := registry.Register(&serverTestProvider{id: "broken", healthErr: errors.New("down")}); err != nil {
		t.Fatalf("Register broken: %v", err)
	}

	s := New(nil, registry, slog.Default(), RateLimitConfig{}, "test-instance", nil, nil, "")
	health, err := s.Health(context.Background(), &bridgev1.HealthRequest{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Status != "serving" || len(health.Providers) != 2 {
		t.Fatalf("Health=%+v", health)
	}

	providers, err := s.ListProviders(context.Background(), &bridgev1.ListProvidersRequest{})
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(providers.Providers) != 2 {
		t.Fatalf("providers len=%d want 2", len(providers.Providers))
	}

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "project-a"})
	claims, err := mustClaims(ctx)
	if err != nil {
		t.Fatalf("mustClaims: %v", err)
	}
	if err := authorizeProject(claims, "project-a"); err != nil {
		t.Fatalf("authorizeProject: %v", err)
	}
	if err := authorizeProject(claims, "project-b"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("authorizeProject mismatch code=%v want %v", status.Code(err), codes.PermissionDenied)
	}
	if _, err := mustClaims(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("mustClaims missing code=%v want %v", status.Code(err), codes.Unauthenticated)
	}

	info := &bridge.SessionInfo{
		SessionID:        "session-a",
		ProjectID:        "project-a",
		Provider:         "healthy",
		State:            bridge.SessionStateAttached,
		CreatedAt:        time.Unix(10, 0),
		StoppedAt:        time.Unix(20, 0),
		Error:            "boom",
		Attached:         true,
		AttachedClientID: "client-a",
		ExitRecorded:     true,
		ExitCode:         9,
		OldestSeq:        1,
		LastSeq:          2,
		Cols:             80,
		Rows:             24,
	}
	resp := sessionInfoToProto(info)
	if resp.GetSessionId() != "session-a" || resp.GetStatus() != bridgev1.SessionStatus_SESSION_STATUS_ATTACHED {
		t.Fatalf("sessionInfoToProto=%+v", resp)
	}

	chunk := chunkToProto("session-a", bridge.OutputChunk{
		Seq:       7,
		Timestamp: time.Unix(30, 0),
		Payload:   []byte("hello"),
	}, true)
	if chunk.GetSeq() != 7 || !chunk.GetReplay() {
		t.Fatalf("chunkToProto=%+v", chunk)
	}

	// ChunkTypeThinking → ATTACH_EVENT_TYPE_THINKING with thinking_text
	thinkChunk := chunkToProto("session-a", bridge.OutputChunk{
		Seq:       8,
		Timestamp: time.Unix(40, 0),
		Payload:   []byte("deep thought"),
		Type:      bridge.ChunkTypeThinking,
	}, false)
	if thinkChunk.GetType() != bridgev1.AttachEventType_ATTACH_EVENT_TYPE_THINKING {
		t.Fatalf("expected THINKING event type, got %v", thinkChunk.GetType())
	}
	if thinkChunk.GetThinkingText() != "deep thought" {
		t.Fatalf("expected thinking_text='deep thought', got %q", thinkChunk.GetThinkingText())
	}
	if len(thinkChunk.GetPayload()) != 0 {
		t.Fatalf("expected empty payload for THINKING event, got %v", thinkChunk.GetPayload())
	}
	if thinkChunk.GetSeq() != 8 {
		t.Fatalf("expected seq=8, got %d", thinkChunk.GetSeq())
	}
	if thinkChunk.GetReplay() {
		t.Fatalf("expected replay=false for THINKING event")
	}
}

func TestMapBridgeErrorAndState(t *testing.T) {
	cases := []struct {
		err  error
		code codes.Code
	}{
		{err: bridge.ErrInvalidArgument, code: codes.InvalidArgument},
		{err: bridge.ErrSessionNotFound, code: codes.NotFound},
		{err: bridge.ErrSessionAlreadyExists, code: codes.AlreadyExists},
		{err: bridge.ErrSessionAlreadyAttached, code: codes.ResourceExhausted},
		{err: bridge.ErrClientMismatch, code: codes.PermissionDenied},
		{err: bridge.ErrProviderUnavailable, code: codes.Unavailable},
		{err: bridge.ErrSessionRecoveryUnavailable, code: codes.Unavailable},
		{err: bridge.ErrSupervisorShuttingDown, code: codes.Unavailable},
		{err: bridge.ErrRepoSetupFailed, code: codes.FailedPrecondition},
		{err: bridge.ErrSessionLimitReached, code: codes.ResourceExhausted},
		{err: errors.New("boom"), code: codes.Internal},
	}
	for _, tc := range cases {
		if got := status.Code(mapBridgeError(tc.err, "op")); got != tc.code {
			t.Fatalf("mapBridgeError(%v) code=%v want %v", tc.err, got, tc.code)
		}
	}

	if got := mapState(bridge.SessionStateFailed); got != bridgev1.SessionStatus_SESSION_STATUS_FAILED {
		t.Fatalf("mapState failed=%v", got)
	}
	if got := mapState(bridge.SessionState(999)); got != bridgev1.SessionStatus_SESSION_STATUS_UNSPECIFIED {
		t.Fatalf("mapState unknown=%v", got)
	}

	errText := mapBridgeError(bridge.ErrProviderUnavailable, "list")
	if !strings.Contains(errText.Error(), "list:") {
		t.Fatalf("mapBridgeError text=%q want op prefix", errText.Error())
	}
}

func newServerWithSupervisor(t *testing.T) (*BridgeServer, *bridge.Supervisor) {
	t.Helper()
	registry := bridge.NewRegistry()
	if err := registry.Register(&serverTestProvider{id: "cat"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sup := bridge.NewSupervisor(registry, bridge.DefaultPolicy(), 1024*1024, time.Minute)
	t.Cleanup(func() { sup.Close() })
	s := New(sup, registry, slog.Default(), RateLimitConfig{}, "test", nil, nil, "")
	return s, sup
}

func startServerSession(t *testing.T, s *BridgeServer, sessionID string) string {
	t.Helper()
	repoPath := t.TempDir()
	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})
	_, err := s.StartSession(ctx, &bridgev1.StartSessionRequest{
		ProjectId: "proj",
		SessionId: sessionID,
		RepoPath:  repoPath,
		Provider:  "cat",
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	return repoPath
}

const (
	testClaimSessionID   = "05877c43-ef0f-4841-95cb-377e7be1a2a0"
	testReleaseSessionID = "2ba8d806-cbff-4827-8b0a-6ec6a80b07a4"
)

func TestClaimWriterRPC(t *testing.T) {
	s, sup := newServerWithSupervisor(t)
	startServerSession(t, s, testClaimSessionID)

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})

	// Use the supervisor directly to establish writer state (AttachSession is streaming).
	if _, err := sup.Attach(testClaimSessionID, "old-writer", 0, bridge.AttachRoleWriter); err != nil {
		t.Fatalf("Attach old-writer: %v", err)
	}
	if _, err := sup.Attach(testClaimSessionID, "new-client", 0, bridge.AttachRoleObserver); err != nil {
		t.Fatalf("Attach new-client observer: %v", err)
	}

	// Force-claim the writer slot via the server RPC.
	resp, err := s.ClaimWriter(ctx, &bridgev1.ClaimWriterRequest{
		SessionId: testClaimSessionID,
		ClientId:  "new-client",
		Force:     true,
	})
	if err != nil {
		t.Fatalf("ClaimWriter: %v", err)
	}
	if resp.GetPreviousWriterClientId() != "old-writer" {
		t.Errorf("PreviousWriterClientId=%q want old-writer", resp.GetPreviousWriterClientId())
	}

	// ClaimWriter on unknown session returns NotFound.
	_, err = s.ClaimWriter(ctx, &bridgev1.ClaimWriterRequest{
		SessionId: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		ClientId:  "some-client",
		Force:     false,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("ClaimWriter unknown session code=%v want NotFound", status.Code(err))
	}
}

func TestReleaseWriterRPC(t *testing.T) {
	s, sup := newServerWithSupervisor(t)
	startServerSession(t, s, testReleaseSessionID)

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})

	if _, err := sup.Attach(testReleaseSessionID, "the-writer", 0, bridge.AttachRoleWriter); err != nil {
		t.Fatalf("Attach writer: %v", err)
	}

	// Release via server RPC.
	if _, err := s.ReleaseWriter(ctx, &bridgev1.ReleaseWriterRequest{
		SessionId: testReleaseSessionID,
		ClientId:  "the-writer",
	}); err != nil {
		t.Fatalf("ReleaseWriter: %v", err)
	}

	info, err := sup.Get(testReleaseSessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.ActiveWriterClientID != "" {
		t.Errorf("ActiveWriterClientID=%q want empty after release", info.ActiveWriterClientID)
	}

	// ReleaseWriter on unknown session returns NotFound.
	_, err = s.ReleaseWriter(ctx, &bridgev1.ReleaseWriterRequest{
		SessionId: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		ClientId:  "some-client",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("ReleaseWriter unknown session code=%v want NotFound", status.Code(err))
	}
}

func TestMapBridgeErrorWriterConflict(t *testing.T) {
	got := status.Code(mapBridgeError(bridge.ErrWriterConflict, "attach"))
	if got != codes.AlreadyExists {
		t.Fatalf("ErrWriterConflict code=%v want AlreadyExists", got)
	}
}

func TestStopWriteResizeRPCs(t *testing.T) {
	s, sup := newServerWithSupervisor(t)
	const sid = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	repoPath := startServerSession(t, s, sid)

	ctx := auth.ContextWithClaims(context.Background(), &auth.BridgeClaims{ProjectID: "proj"})

	if _, err := sup.Attach(sid, "cli", 0, bridge.AttachRoleWriter); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// WriteInput happy path.
	if _, err := s.WriteInput(ctx, &bridgev1.WriteInputRequest{
		SessionId: sid,
		ClientId:  "cli",
		Data:      []byte("hello"),
	}); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	// ResizeSession happy path.
	if _, err := s.ResizeSession(ctx, &bridgev1.ResizeSessionRequest{
		SessionId: sid,
		ClientId:  "cli",
		Cols:      100,
		Rows:      40,
	}); err != nil {
		t.Fatalf("ResizeSession: %v", err)
	}

	// ResizeSession invalid (zero cols).
	_, err := s.ResizeSession(ctx, &bridgev1.ResizeSessionRequest{
		SessionId: sid,
		ClientId:  "cli",
		Cols:      0,
		Rows:      40,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ResizeSession zero cols code=%v want InvalidArgument", status.Code(err))
	}

	// GetSession happy path.
	resp, err := s.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: sid})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if resp.GetSessionId() != sid {
		t.Errorf("GetSession id=%q want %q", resp.GetSessionId(), sid)
	}
	if resp.GetRepoPath() != repoPath {
		t.Errorf("GetSession repo_path=%q want %q", resp.GetRepoPath(), repoPath)
	}

	// StopSession happy path.
	if _, err := s.StopSession(ctx, &bridgev1.StopSessionRequest{SessionId: sid}); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
}

func TestGenerateID(t *testing.T) {
	id1 := generateID()
	id2 := generateID()
	if id1 == "" || id2 == "" {
		t.Fatal("generateID returned empty string")
	}
	if id1 == id2 {
		t.Fatal("generateID returned duplicate IDs")
	}
}

func TestValidateFieldEdgeCases(t *testing.T) {
	// validateStringField: too long
	if err := validateStringField("f", strings.Repeat("x", 300), 256, false); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("too-long string code=%v want InvalidArgument", status.Code(err))
	}
	// validateStringField: invalid UTF-8
	if err := validateStringField("f", "\xff\xfe", 256, false); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid utf8 code=%v want InvalidArgument", status.Code(err))
	}
	// validateStringField: control char disallowed
	if err := validateStringField("f", "foo\x01bar", 256, false); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("control char code=%v want InvalidArgument", status.Code(err))
	}
	// validateStringField: whitespace control allowed
	if err := validateStringField("f", "foo\nbar", 256, true); err != nil {
		t.Fatalf("whitespace control disallowed: %v", err)
	}
	// validateByteField: empty
	if err := validateByteField("d", []byte{}, 10); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty bytes code=%v want InvalidArgument", status.Code(err))
	}
	// validateByteField: too large
	if err := validateByteField("d", make([]byte, 20), 10); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("too-large bytes code=%v want InvalidArgument", status.Code(err))
	}
}

func TestMapStateAllVariants(t *testing.T) {
	cases := []struct {
		in  bridge.SessionState
		out bridgev1.SessionStatus
	}{
		{bridge.SessionStateStarting, bridgev1.SessionStatus_SESSION_STATUS_STARTING},
		{bridge.SessionStateRunning, bridgev1.SessionStatus_SESSION_STATUS_RUNNING},
		{bridge.SessionStateAttached, bridgev1.SessionStatus_SESSION_STATUS_ATTACHED},
		{bridge.SessionStateStopping, bridgev1.SessionStatus_SESSION_STATUS_STOPPING},
		{bridge.SessionStateStopped, bridgev1.SessionStatus_SESSION_STATUS_STOPPED},
	}
	for _, tc := range cases {
		got := mapState(tc.in)
		if got != tc.out {
			t.Errorf("mapState(%v)=%v want %v", tc.in, got, tc.out)
		}
	}
}
