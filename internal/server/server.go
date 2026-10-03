package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/auth"
	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/enrollment"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

type BridgeServer struct {
	bridgev1.UnimplementedBridgeServiceServer

	supervisor       *bridge.Supervisor
	registry         *bridge.Registry
	logger           *slog.Logger
	globalRL         *keyedLimiter
	startRL          *keyedLimiter
	writeRL          *keyedLimiter
	serverInstanceID string
	// providerFallbacks maps each provider ID to its ordered fallback list.
	providerFallbacks map[string][]string
	// jwtVerifier is the live JWT verifier for hot-adding keys via
	// RegisterJWTKey. Nil in local (passthrough auth) mode.
	jwtVerifier *auth.JWTVerifier
	// certsDir is the certs directory for persisting enrolled JWT public keys.
	certsDir string
	// diagnoseStateDir locates the control client files read by DiagnoseSession.
	diagnoseStateDir string
	// enrollStore is the enrollment token store. Nil when enrollment is
	// not configured.
	enrollStore *enrollment.Store
	// version is the running daemon's build version, reported back to
	// clients via Health so `bridgectl doctor` can detect a stale daemon
	// after an upgrade. Empty for unversioned dev builds.
	version string
}

type RateLimitConfig struct {
	GlobalRPS                  float64
	GlobalBurst                int
	StartSessionPerClientRPS   float64
	StartSessionPerClientBurst int
	SendInputPerSessionRPS     float64
	SendInputPerSessionBurst   int
}

func New(supervisor *bridge.Supervisor, registry *bridge.Registry, logger *slog.Logger, rl RateLimitConfig, serverInstanceID string, providerFallbacks map[string][]string, jwtVerifier *auth.JWTVerifier, certsDir string, version string) *BridgeServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &BridgeServer{
		supervisor:        supervisor,
		registry:          registry,
		logger:            logger,
		globalRL:          newKeyedLimiter(rl.GlobalRPS, rl.GlobalBurst),
		startRL:           newKeyedLimiter(rl.StartSessionPerClientRPS, rl.StartSessionPerClientBurst),
		writeRL:           newKeyedLimiter(rl.SendInputPerSessionRPS, rl.SendInputPerSessionBurst),
		serverInstanceID:  serverInstanceID,
		providerFallbacks: providerFallbacks,
		jwtVerifier:       jwtVerifier,
		certsDir:          certsDir,
		version:           version,
	}
}

// SetDiagnoseStateDir sets the state directory DiagnoseSession reads the
// control client's status and revision files from. When unset, diagnostics
// report control status as unknown and wire revisions as null.
func (s *BridgeServer) SetDiagnoseStateDir(dir string) {
	s.diagnoseStateDir = dir
}

// SetEnrollmentStore configures the enrollment token store for the
// EnrollClient RPC. When nil, enrollment is not available.
func (s *BridgeServer) SetEnrollmentStore(store *enrollment.Store) {
	s.enrollStore = store
}

func (s *BridgeServer) StartSession(ctx context.Context, req *bridgev1.StartSessionRequest) (*bridgev1.StartSessionResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateStringField("project_id", req.ProjectId, maxProjectIDLen, false); err != nil {
		return nil, err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := validateStringField("repo_path", req.RepoPath, maxRepoPathLen, false); err != nil {
		return nil, err
	}
	if err := validateStringField("provider", req.Provider, maxProviderLen, false); err != nil {
		return nil, err
	}
	if err := authorizeProject(claims, req.ProjectId); err != nil {
		return nil, err
	}

	clientID := claims.Subject
	if clientID == "" {
		clientID = claims.ProjectID
	}
	if !s.startRL.allow(clientID) {
		return nil, status.Error(codes.ResourceExhausted, "start session rate limit exceeded for client")
	}

	if err := checkDirReadWrite(req.RepoPath); err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "repo_path %q: %v", req.RepoPath, err)
	}

	opts := map[string]string{"provider": req.Provider}
	for k, v := range req.AgentOpts {
		opts[k] = v
	}

	s.logger.Info("starting session", "session_id", req.SessionId, "project_id", req.ProjectId, "provider", req.Provider, "repo_path", req.RepoPath)
	info, err := s.supervisor.Start(ctx, bridge.SessionConfig{
		SessionID:   req.SessionId,
		ProjectID:   req.ProjectId,
		RepoPath:    req.RepoPath,
		Options:     opts,
		Fallbacks:   s.providerFallbacks[req.Provider],
		InitialCols: req.InitialCols,
		InitialRows: req.InitialRows,
	})
	if err != nil {
		s.logger.Warn("start session failed", "session_id", req.SessionId, "error", err)
		return nil, mapBridgeError(err, "start session")
	}
	s.logger.Info("session started", "session_id", info.SessionID, "provider", info.Provider, "pid", info.ProcessID)
	return &bridgev1.StartSessionResponse{
		SessionId: info.SessionID,
		Status:    mapState(info.State),
		CreatedAt: timestamppb.New(info.CreatedAt),
	}, nil
}

func (s *BridgeServer) StopSession(ctx context.Context, req *bridgev1.StopSessionRequest) (*bridgev1.StopSessionResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := s.authorizeSession(claims, req.SessionId); err != nil {
		return nil, err
	}
	s.logger.Info("stopping session", "session_id", req.SessionId, "force", req.Force)
	if err := s.supervisor.Stop(req.SessionId, req.Force); err != nil {
		s.logger.Warn("stop session failed", "session_id", req.SessionId, "error", err)
		return nil, mapBridgeError(err, "stop session")
	}
	return &bridgev1.StopSessionResponse{Status: bridgev1.SessionStatus_SESSION_STATUS_STOPPING}, nil
}

func (s *BridgeServer) GetSession(ctx context.Context, req *bridgev1.GetSessionRequest) (*bridgev1.GetSessionResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := s.authorizeSession(claims, req.SessionId); err != nil {
		return nil, err
	}
	info, err := s.supervisor.Get(req.SessionId)
	if err != nil {
		return nil, mapBridgeError(err, "get session")
	}
	return sessionInfoToProto(info), nil
}

// DiagnoseSession returns the schema-versioned diagnostic snapshot of one
// session. It is authorized and rate limited exactly like GetSession, reads
// only Supervisor state plus the control client's local files, and never
// consults Bridge, telemetry or terminal output.
func (s *BridgeServer) DiagnoseSession(ctx context.Context, req *bridgev1.DiagnoseSessionRequest) (*bridgev1.DiagnoseSessionResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := s.authorizeSession(claims, req.SessionId); err != nil {
		return nil, err
	}
	info, err := s.supervisor.Get(req.SessionId)
	if err != nil {
		return nil, mapBridgeError(err, "diagnose session")
	}
	b, err := DiagnosticReportJSON(info, s.diagnoseStateDir, s.version)
	if err != nil {
		return nil, status.Error(codes.Internal, "diagnose session: serialize report")
	}
	return &bridgev1.DiagnoseSessionResponse{ReportJson: b}, nil
}

func (s *BridgeServer) ListSessions(ctx context.Context, req *bridgev1.ListSessionsRequest) (*bridgev1.ListSessionsResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	projectID := req.ProjectId
	if claims.ProjectID != "" {
		if projectID != "" && projectID != claims.ProjectID {
			return nil, status.Errorf(codes.PermissionDenied, "token project_id %q does not match request %q", claims.ProjectID, projectID)
		}
		projectID = claims.ProjectID
	}
	items := s.supervisor.List(projectID)
	resp := &bridgev1.ListSessionsResponse{
		Sessions: make([]*bridgev1.GetSessionResponse, 0, len(items)),
	}
	for i := range items {
		info := items[i]
		resp.Sessions = append(resp.Sessions, sessionInfoToProto(&info))
	}
	return resp, nil
}

func (s *BridgeServer) AttachSession(req *bridgev1.AttachSessionRequest, stream bridgev1.BridgeService_AttachSessionServer) error {
	if !s.globalRL.allow("global") {
		return status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(stream.Context())
	if err != nil {
		return err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return err
	}
	if err := validateOptionalStringField("client_id", req.ClientId, maxSessionIDLen, false); err != nil {
		return err
	}
	if err := s.authorizeSession(claims, req.SessionId); err != nil {
		return err
	}
	clientID := req.ClientId
	if clientID == "" {
		clientID = generateID()
	}
	role := bridge.AttachRoleWriter
	if req.Role == bridgev1.AttachRole_ATTACH_ROLE_OBSERVER {
		role = bridge.AttachRoleObserver
	}
	s.logger.Info("attaching to session", "session_id", req.SessionId, "client_id", clientID, "after_seq", req.AfterSeq, "role", role)
	state, err := s.supervisor.Attach(req.SessionId, clientID, req.AfterSeq, role)
	if err != nil {
		s.logger.Warn("attach session failed", "session_id", req.SessionId, "client_id", clientID, "error", err)
		return mapBridgeError(err, "attach session")
	}
	s.logger.Info("session attached", "session_id", req.SessionId, "client_id", clientID, "replay_chunks", len(state.Replay), "replay_gap", state.ReplayGap)
	defer func() {
		wasWriter, _ := s.supervisor.Detach(req.SessionId, clientID)
		if wasWriter {
			s.supervisor.NotifyWriterReleased(req.SessionId, clientID)
		}
		s.logger.Info("session detached", "session_id", req.SessionId, "client_id", clientID)
	}()

	if err := stream.Send(&bridgev1.AttachSessionEvent{
		Type:         bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED,
		SessionId:    req.SessionId,
		OldestSeq:    state.OldestSeq,
		LastSeq:      state.LastSeq,
		ExitRecorded: state.ExitRecorded,
		ExitCode:     int32(state.ExitCode),
		Cols:         state.Cols,
		Rows:         state.Rows,
	}); err != nil {
		return err
	}
	if state.ReplayGap {
		if err := stream.Send(&bridgev1.AttachSessionEvent{
			Type:      bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP,
			SessionId: req.SessionId,
			OldestSeq: state.OldestSeq,
			LastSeq:   state.LastSeq,
		}); err != nil {
			return err
		}
	}
	lastSeq := req.AfterSeq
	for _, chunk := range state.Replay {
		if err := stream.Send(chunkToProto(req.SessionId, chunk, true)); err != nil {
			return err
		}
		lastSeq = chunk.Seq
	}
	for {
		select {
		case <-stream.Context().Done():
			s.logger.Info("attach stream context done", "session_id", req.SessionId, "client_id", clientID)
			return nil
		case chunk, ok := <-state.Live:
			if !ok {
				// Agent process exited; send a SESSION_EXIT event so
				// the client learns the exit code without a separate
				// GetSession call.  The live channel closes from the
				// read-loop goroutine while waitLoop records the exit
				// code concurrently, so we poll briefly for the exit
				// to be recorded.
				exitEvt := &bridgev1.AttachSessionEvent{
					Type:      bridgev1.AttachEventType_ATTACH_EVENT_TYPE_SESSION_EXIT,
					SessionId: req.SessionId,
				}
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					if info, err := s.supervisor.Get(req.SessionId); err == nil && info.ExitRecorded {
						exitEvt.ExitRecorded = true
						exitEvt.ExitCode = int32(info.ExitCode)
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				s.logger.Info("agent process exited", "session_id", req.SessionId, "client_id", clientID, "exit_code", exitEvt.ExitCode, "exit_recorded", exitEvt.ExitRecorded)
				if err := stream.Send(exitEvt); err != nil {
					s.logger.Warn("failed to send session exit event", "session_id", req.SessionId, "client_id", clientID, "error", err)
				}
				return nil
			}
			isControl := chunk.Type == bridge.ChunkTypeWriterClaimed || chunk.Type == bridge.ChunkTypeWriterReleased
			if !isControl {
				if chunk.Seq <= lastSeq {
					continue
				}
				lastSeq = chunk.Seq
			}
			if err := stream.Send(chunkToProto(req.SessionId, chunk, false)); err != nil {
				return err
			}
		}
	}
}

func (s *BridgeServer) WriteInput(ctx context.Context, req *bridgev1.WriteInputRequest) (*bridgev1.WriteInputResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := validateStringField("client_id", req.ClientId, maxSessionIDLen, false); err != nil {
		return nil, err
	}
	if err := validateByteField("data", req.Data, 1<<20); err != nil {
		return nil, err
	}
	if err := s.authorizeSession(claims, req.SessionId); err != nil {
		return nil, err
	}
	if !s.writeRL.allow(req.SessionId) {
		if s.writeRL.banned(req.SessionId) {
			// Circuit breaker tripped: the client has sent too many requests while
			// rate-limited. Force-terminate the session so the client can no longer
			// write to it, then return Unavailable (distinct from ResourceExhausted)
			// so a well-behaved client stops retrying entirely.
			s.logger.Warn("write input circuit breaker tripped, terminating session",
				"session_id", req.SessionId, "client_id", req.ClientId)
			go func() { _ = s.supervisor.Stop(req.SessionId, true) }()
			return nil, status.Error(codes.Unavailable, "write input circuit breaker: session terminated due to sustained rate limit violations")
		}
		return nil, status.Error(codes.ResourceExhausted, "write input rate limit exceeded for session")
	}
	n, err := s.supervisor.WriteInput(req.SessionId, req.ClientId, req.Data)
	if err != nil {
		return nil, mapBridgeError(err, "write input")
	}
	return &bridgev1.WriteInputResponse{Accepted: true, BytesWritten: uint32(n)}, nil
}

func (s *BridgeServer) ResizeSession(ctx context.Context, req *bridgev1.ResizeSessionRequest) (*bridgev1.ResizeSessionResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := validateStringField("client_id", req.ClientId, maxSessionIDLen, false); err != nil {
		return nil, err
	}
	if req.Cols == 0 || req.Rows == 0 {
		return nil, status.Error(codes.InvalidArgument, "cols and rows must be > 0")
	}
	if err := s.authorizeSession(claims, req.SessionId); err != nil {
		return nil, err
	}
	if err := s.supervisor.Resize(req.SessionId, req.ClientId, req.Cols, req.Rows); err != nil {
		return nil, mapBridgeError(err, "resize session")
	}
	return &bridgev1.ResizeSessionResponse{Applied: true}, nil
}

func mustClaims(ctx context.Context) (*auth.BridgeClaims, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing claims")
	}
	return claims, nil
}

func authorizeProject(claims *auth.BridgeClaims, projectID string) error {
	if claims.ProjectID != "" && claims.ProjectID != projectID {
		return status.Errorf(codes.PermissionDenied, "token project_id %q does not match request %q", claims.ProjectID, projectID)
	}
	return nil
}

func (s *BridgeServer) authorizeSession(claims *auth.BridgeClaims, sessionID string) error {
	info, err := s.supervisor.Get(sessionID)
	if err != nil {
		return mapBridgeError(err, "authorize session")
	}
	return authorizeProject(claims, info.ProjectID)
}

func mapBridgeError(err error, op string) error {
	switch {
	case errors.Is(err, bridge.ErrInvalidArgument), errors.Is(err, bridge.ErrSessionNotRunning):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, bridge.ErrSessionNotFound):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, bridge.ErrSessionAlreadyExists), errors.Is(err, bridge.ErrWriterConflict):
		return status.Errorf(codes.AlreadyExists, "%s: %v", op, err)
	case errors.Is(err, bridge.ErrSessionAlreadyAttached), errors.Is(err, bridge.ErrInputTooLarge):
		return status.Errorf(codes.ResourceExhausted, "%s: %v", op, err)
	case errors.Is(err, bridge.ErrClientNotAttached), errors.Is(err, bridge.ErrClientMismatch):
		return status.Errorf(codes.PermissionDenied, "%s: %v", op, err)
	case errors.Is(err, bridge.ErrProviderUnavailable), errors.Is(err, bridge.ErrSessionRecoveryUnavailable), errors.Is(err, bridge.ErrSupervisorShuttingDown):
		return status.Errorf(codes.Unavailable, "%s: %v", op, err)
	case errors.Is(err, bridge.ErrRepoSetupFailed):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, bridge.ErrSessionLimitReached):
		return status.Errorf(codes.ResourceExhausted, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

func (s *BridgeServer) Health(ctx context.Context, req *bridgev1.HealthRequest) (*bridgev1.HealthResponse, error) {
	results := s.registry.HealthAll(ctx)
	providers := make([]*bridgev1.ProviderHealth, 0, len(results))
	for id, err := range results {
		item := &bridgev1.ProviderHealth{Provider: id, Available: err == nil}
		if err != nil {
			item.Error = err.Error()
		}
		providers = append(providers, item)
	}
	return &bridgev1.HealthResponse{
		Status:           "serving",
		Providers:        providers,
		ServerInstanceId: s.serverInstanceID,
		ServerVersion:    s.version,
	}, nil
}

func (s *BridgeServer) ClaimWriter(ctx context.Context, req *bridgev1.ClaimWriterRequest) (*bridgev1.ClaimWriterResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := s.authorizeSession(claims, req.SessionId); err != nil {
		return nil, err
	}
	clientID := req.ClientId
	if clientID == "" {
		return nil, status.Error(codes.InvalidArgument, "client_id is required")
	}
	result, err := s.supervisor.ClaimWriter(req.SessionId, clientID, req.Force)
	if err != nil {
		return nil, mapBridgeError(err, "claim writer")
	}
	// Notify the evicted writer before announcing the new one so observers see
	// a consistent released→claimed sequence.
	if result.PreviousWriterClientID != "" {
		s.supervisor.NotifyWriterReleased(req.SessionId, result.PreviousWriterClientID)
	}
	s.supervisor.NotifyWriterClaimed(req.SessionId, clientID)
	return &bridgev1.ClaimWriterResponse{
		Claimed:                true,
		PreviousWriterClientId: result.PreviousWriterClientID,
	}, nil
}

func (s *BridgeServer) ReleaseWriter(ctx context.Context, req *bridgev1.ReleaseWriterRequest) (*bridgev1.ReleaseWriterResponse, error) {
	if !s.globalRL.allow("global") {
		return nil, status.Error(codes.ResourceExhausted, "global RPC rate limit exceeded")
	}
	claims, err := mustClaims(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateUUIDField("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := s.authorizeSession(claims, req.SessionId); err != nil {
		return nil, err
	}
	clientID := req.ClientId
	if clientID == "" {
		return nil, status.Error(codes.InvalidArgument, "client_id is required")
	}
	if err := s.supervisor.ReleaseWriter(req.SessionId, clientID); err != nil {
		return nil, mapBridgeError(err, "release writer")
	}
	s.supervisor.NotifyWriterReleased(req.SessionId, clientID)
	return &bridgev1.ReleaseWriterResponse{Released: true}, nil
}

func (s *BridgeServer) ListProviders(ctx context.Context, req *bridgev1.ListProvidersRequest) (*bridgev1.ListProvidersResponse, error) {
	ids := s.registry.List()
	results := s.registry.HealthAll(ctx)
	items := make([]*bridgev1.ProviderInfo, 0, len(ids))
	for _, id := range ids {
		var version string
		if p, err := s.registry.Get(id); err == nil && results[id] == nil {
			version, _ = p.Version(ctx)
		}
		items = append(items, &bridgev1.ProviderInfo{
			Provider:  id,
			Available: results[id] == nil,
			Binary:    "",
			Version:   version,
		})
	}
	return &bridgev1.ListProvidersResponse{Providers: items}, nil
}

func sessionInfoToProto(info *bridge.SessionInfo) *bridgev1.GetSessionResponse {
	resp := &bridgev1.GetSessionResponse{
		SessionId:            info.SessionID,
		ProjectId:            info.ProjectID,
		Provider:             info.Provider,
		RepoPath:             info.RepoPath,
		Status:               mapState(info.State),
		CreatedAt:            timestamppb.New(info.CreatedAt),
		Error:                info.Error,
		Attached:             info.Attached,
		AttachedClientId:     info.AttachedClientID,
		ExitRecorded:         info.ExitRecorded,
		ExitCode:             int32(info.ExitCode),
		OldestSeq:            info.OldestSeq,
		LastSeq:              info.LastSeq,
		Cols:                 info.Cols,
		Rows:                 info.Rows,
		ActiveWriterClientId: info.ActiveWriterClientID,
		ObserverCount:        int32(info.ObserverCount),
		Interaction:          interactionToProto(info.Interaction),
	}
	if !info.StoppedAt.IsZero() {
		resp.StoppedAt = timestamppb.New(info.StoppedAt)
	}
	return resp
}

func mapState(s bridge.SessionState) bridgev1.SessionStatus {
	switch s {
	case bridge.SessionStateStarting:
		return bridgev1.SessionStatus_SESSION_STATUS_STARTING
	case bridge.SessionStateRunning:
		return bridgev1.SessionStatus_SESSION_STATUS_RUNNING
	case bridge.SessionStateAttached:
		return bridgev1.SessionStatus_SESSION_STATUS_ATTACHED
	case bridge.SessionStateStopping:
		return bridgev1.SessionStatus_SESSION_STATUS_STOPPING
	case bridge.SessionStateStopped:
		return bridgev1.SessionStatus_SESSION_STATUS_STOPPED
	case bridge.SessionStateFailed:
		return bridgev1.SessionStatus_SESSION_STATUS_FAILED
	default:
		return bridgev1.SessionStatus_SESSION_STATUS_UNSPECIFIED
	}
}

// checkDirReadWrite verifies that dir exists, is a directory, and is writable
// by the current process. Returns an error if any check fails so that
// StartSession can reject requests before spawning a provider process.
func checkDirReadWrite(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	f, err := os.CreateTemp(dir, ".bridge-rw-check-*")
	if err != nil {
		return fmt.Errorf("not writable: %w", err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return nil
}

func chunkToProto(sessionID string, chunk bridge.OutputChunk, replay bool) *bridgev1.AttachSessionEvent {
	ev := &bridgev1.AttachSessionEvent{
		Type:      bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT,
		Seq:       chunk.Seq,
		Timestamp: timestamppb.New(chunk.Timestamp),
		SessionId: sessionID,
		Payload:   chunk.Payload,
		Replay:    replay,
	}
	switch chunk.Type {
	case bridge.ChunkTypeThinking:
		ev.Type = bridgev1.AttachEventType_ATTACH_EVENT_TYPE_THINKING
		ev.ThinkingText = string(chunk.Payload)
		ev.Payload = nil
	case bridge.ChunkTypeWriterClaimed:
		ev.Type = bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_CLAIMED
		ev.WriterClientId = string(chunk.Payload)
		ev.Payload = nil
	case bridge.ChunkTypeWriterReleased:
		ev.Type = bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED
		ev.WriterClientId = string(chunk.Payload)
		ev.Payload = nil
	}
	return ev
}
