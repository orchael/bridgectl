package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/orchael/bridgectl/internal/telemetry"
)

// ansiEscape matches ANSI/VT100 escape sequences (CSI sequences and 2-char
// escape sequences) so they can be stripped from PTY output when needed.
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-9;?=<>]*[a-zA-Z~]|[@-Z\x5c-_])`)

// AttachRole controls whether the attaching client can send input (Writer) or
// is read-only (Observer).
type AttachRole int

const (
	// AttachRoleWriter requests the active-writer slot. Fails with
	// ErrWriterConflict when another client already holds the slot.
	AttachRoleWriter AttachRole = iota
	// AttachRoleObserver attaches read-only; WriteInput / Resize are rejected.
	AttachRoleObserver
)

// AttachState is returned by Supervisor.Attach and holds the replay buffer,
// live output channel, and session metadata for the attaching client.
type AttachState struct {
	ClientID     string
	Role         AttachRole
	Replay       []OutputChunk
	Live         <-chan OutputChunk
	ReplayGap    bool
	OldestSeq    uint64
	LastSeq      uint64
	ExitRecorded bool
	ExitCode     int
	Cols         uint32
	Rows         uint32
}

// observerEntry holds the live channel for a single attached client.
type observerEntry struct {
	ch   chan OutputChunk
	role AttachRole
}

// SupervisorOption configures optional Supervisor behaviour.
type SupervisorOption func(*Supervisor)

type TelemetryObserver interface {
	SessionStarted(telemetry.Session)
	ObserveProviderChunk(telemetry.Session, telemetry.StreamType, []byte)
	ObserveInputChunk(telemetry.Session, []byte)
	SessionEnded(telemetry.Session)
	Close(context.Context) error
}

func WithTelemetry(observer TelemetryObserver) SupervisorOption {
	return func(s *Supervisor) { s.telemetry = observer }
}

// WithStore attaches a SessionStore so that session metadata is persisted on
// every terminal state transition and reloaded at startup via LoadHistory.
func WithStore(store SessionStore) SupervisorOption {
	return func(s *Supervisor) {
		s.store = store
	}
}

func WithRepoSetupRunner(runner RepoSetupRunner) SupervisorOption {
	return func(s *Supervisor) {
		s.repoSetup = runner
	}
}

// Supervisor manages the lifecycle of PTY-backed provider sessions.
type Supervisor struct {
	registry        *Registry
	policy          Policy
	bufSize         int
	idleTimeout     time.Duration
	cleanupInterval time.Duration

	mu                 sync.RWMutex
	sessions           map[string]*managedSession
	done               chan struct{}
	closeOnce          sync.Once
	telemetryCloseOnce sync.Once
	shuttingDown       bool

	store     SessionStore
	repoSetup RepoSetupRunner
	histMu    sync.RWMutex
	history   map[string]SessionInfo
	telemetry TelemetryObserver
}

type managedSession struct {
	mu           sync.Mutex
	info         SessionInfo
	provider     Provider
	cmd          *exec.Cmd
	ptmx         *os.File       // non-nil for PTY-backed sessions
	stdin        io.WriteCloser // non-nil for stream-JSON sessions
	streamJSON   bool           // true when provider uses stream-JSON mode
	buf          *ByteBuffer
	cancel       context.CancelFunc
	stopGrace    time.Duration
	lastActivity time.Time
	forceStop    bool
	recovered    bool

	stripANSI bool // strip ANSI escape codes from PTY output before forwarding

	// Multi-observer state. All fields below are protected by ms.mu.
	//
	// observers holds all currently attached clients keyed by clientID.
	// The writer (if any) is always in observers too — activeWriter names it.
	observers  map[string]*observerEntry
	liveClosed bool // set by closeLive; new observers receive a pre-closed channel
}

func NewSupervisor(registry *Registry, policy Policy, outputBufSize int, idleTimeout time.Duration, opts ...SupervisorOption) *Supervisor {
	if outputBufSize <= 0 {
		outputBufSize = 8 << 20
	}
	s := &Supervisor{
		registry:        registry,
		policy:          policy,
		bufSize:         outputBufSize,
		idleTimeout:     idleTimeout,
		cleanupInterval: 30 * time.Second,
		sessions:        make(map[string]*managedSession),
		done:            make(chan struct{}),
		history:         make(map[string]SessionInfo),
	}
	for _, opt := range opts {
		opt(s)
	}
	go s.cleanupLoop()
	return s
}

// LoadHistory reads all persisted sessions from the store and places them in
// the in-memory history map so they are visible via Get and List. Sessions
// that were not in a terminal state (i.e. the daemon crashed mid-flight) are
// marked as SessionStateFailed with an "orphaned by daemon restart" message
// and their updated state is written back to the store.
//
// Call LoadHistory once, before serving requests.
func (s *Supervisor) LoadHistory() error {
	if s.store == nil {
		return nil
	}
	infos, err := s.store.LoadAll()
	if err != nil {
		return err
	}
	s.histMu.Lock()
	defer s.histMu.Unlock()
	for _, info := range infos {
		if info.State != SessionStateStopped && info.State != SessionStateFailed {
			if s.recoverProcess(&info) {
				continue
			}
			info.State = SessionStateFailed
			if info.Error == "" {
				info.Error = "orphaned by daemon restart"
			}
			if info.StoppedAt.IsZero() {
				info.StoppedAt = nowUTC()
			}
			// Best-effort: ignore write errors during startup.
			if saveErr := s.store.Save(info); saveErr != nil {
				slog.Warn("session store: failed to update orphaned session", "session_id", info.SessionID, "error", saveErr)
			}
		}
		s.history[info.SessionID] = info
	}
	return nil
}

func (s *Supervisor) recoverProcess(info *SessionInfo) bool {
	if info.ProcessID <= 0 || !processAlive(info.ProcessID) {
		return false
	}

	ms := &managedSession{
		info: SessionInfo{
			SessionID:    info.SessionID,
			ProjectID:    info.ProjectID,
			Provider:     info.Provider,
			State:        SessionStateRunning,
			ProcessID:    info.ProcessID,
			CreatedAt:    info.CreatedAt,
			Error:        "recovered after daemon restart; live attach/input unavailable",
			Recovered:    true,
			ExitRecorded: info.ExitRecorded,
			ExitCode:     info.ExitCode,
			Cols:         info.Cols,
			Rows:         info.Rows,
		},
		buf:          NewByteBuffer(s.bufSize),
		stopGrace:    500 * time.Millisecond,
		lastActivity: time.Now(),
		recovered:    true,
	}

	if chunks, err := s.store.LoadChunks(info.SessionID); err == nil {
		for _, chunk := range chunks {
			ms.buf.AppendChunk(chunk)
		}
	} else {
		slog.Warn("session store: failed to load chunks for recovered session", "session_id", info.SessionID, "error", err)
	}
	ms.info.OldestSeq = ms.buf.OldestSeq()
	ms.info.LastSeq = ms.buf.LastSeq()

	s.mu.Lock()
	s.sessions[info.SessionID] = ms
	s.mu.Unlock()
	s.persistSession(ms.snapshotInfo())
	go s.monitorRecoveredProcess(ms)
	return true
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (s *Supervisor) monitorRecoveredProcess(ms *managedSession) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			ms.mu.Lock()
			pid := ms.info.ProcessID
			state := ms.info.State
			ms.mu.Unlock()
			if state == SessionStateStopped || state == SessionStateFailed {
				return
			}
			if processAlive(pid) {
				continue
			}
			ms.mu.Lock()
			if ms.info.State != SessionStateStopped && ms.info.State != SessionStateFailed {
				ms.info.State = SessionStateStopped
				ms.info.StoppedAt = nowUTC()
				ms.info.ProcessID = 0
			}
			ms.mu.Unlock()
			s.persistSession(ms.snapshotInfo())
			return
		}
	}
}

// persistSession writes info to the store if one is configured. Errors are
// logged at warn level and do not propagate — persistence is best-effort.
func (s *Supervisor) persistSession(info SessionInfo) {
	if s.store == nil {
		return
	}
	if err := s.store.Save(info); err != nil {
		slog.Warn("session store: failed to persist session", "session_id", info.SessionID, "error", err)
	}
}

// persistChunk writes a single PTY output chunk to the store. Errors are
// logged at warn level and do not propagate — persistence is best-effort.
func (s *Supervisor) persistChunk(sessionID string, chunk OutputChunk) {
	if s.store == nil {
		return
	}
	if err := s.store.SaveChunk(sessionID, chunk); err != nil {
		slog.Warn("session store: failed to persist chunk", "session_id", sessionID, "seq", chunk.Seq, "error", err)
	}
}

// attachHistory serves a read-only replay for a session that exists only in
// the persisted history (i.e. from a previous daemon lifetime). Returns
// ErrSessionNotFound if the session is not in history or has no store.
func (s *Supervisor) attachHistory(sessionID, clientID string, afterSeq uint64) (*AttachState, error) {
	if s.store == nil {
		return nil, ErrSessionNotFound
	}
	s.histMu.RLock()
	info, ok := s.history[sessionID]
	s.histMu.RUnlock()
	if !ok {
		return nil, ErrSessionNotFound
	}
	chunks, err := s.store.LoadChunks(sessionID)
	if err != nil {
		return nil, fmt.Errorf("load chunks for %q: %w", sessionID, err)
	}
	var replay []OutputChunk
	for _, c := range chunks {
		if c.Seq > afterSeq {
			replay = append(replay, c)
		}
	}
	var oldest, last uint64
	if len(chunks) > 0 {
		oldest = chunks[0].Seq
		last = chunks[len(chunks)-1].Seq
	}
	// A closed channel signals EOF immediately to the server's streaming loop.
	closed := make(chan OutputChunk)
	close(closed)
	return &AttachState{
		ClientID:     clientID,
		Replay:       replay,
		Live:         closed,
		ReplayGap:    oldest > 0 && afterSeq > 0 && afterSeq < oldest-1,
		OldestSeq:    oldest,
		LastSeq:      last,
		ExitRecorded: info.ExitRecorded,
		ExitCode:     info.ExitCode,
		Cols:         info.Cols,
		Rows:         info.Rows,
	}, nil
}

func (s *Supervisor) cleanupLoop() {
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			// No-op: sessions are only stopped explicitly via Stop() or
			// when the supervisor shuts down via Close(). The idle timeout
			// field is retained for future use but does not reap running
			// or attached sessions.
		}
	}
}

// resolveProvider tries the primary provider ID, then each fallback in order,
// returning the first one that is registered and passes its Health check. If
// no candidate succeeds, the last error is returned.
func (s *Supervisor) resolveProvider(ctx context.Context, primary string, fallbacks []string, env []string) (Provider, error) {
	candidates := make([]string, 0, 1+len(fallbacks))
	candidates = append(candidates, primary)
	candidates = append(candidates, fallbacks...)
	var lastErr error
	for _, id := range candidates {
		p, err := s.registry.Get(id)
		if err != nil {
			lastErr = err
			continue
		}
		var healthErr error
		if hp, ok := p.(EnvHealthProvider); ok && env != nil {
			healthErr = hp.HealthWithEnv(ctx, env)
		} else {
			healthErr = p.Health(ctx)
		}
		if healthErr != nil {
			lastErr = fmt.Errorf("%w: %v", ErrProviderUnavailable, healthErr)
			slog.Warn("provider unavailable, trying fallback", "provider", id, "error", healthErr)
			continue
		}
		if id != primary {
			slog.Info("using fallback provider", "requested", primary, "selected", id)
		}
		return p, nil
	}
	return nil, lastErr
}

func trimSetupError(err error) string {
	msg := err.Error()
	return strings.TrimPrefix(msg, ErrRepoSetupFailed.Error()+": ")
}

func (s *Supervisor) Start(ctx context.Context, cfg SessionConfig) (*SessionInfo, error) {
	if cfg.SessionID == "" {
		return nil, fmt.Errorf("%w: session_id is required", ErrInvalidArgument)
	}
	if cfg.ProjectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrInvalidArgument)
	}
	if cfg.RepoPath == "" {
		return nil, fmt.Errorf("%w: repo_path is required", ErrInvalidArgument)
	}
	if err := s.policy.ValidateRepoPath(cfg.RepoPath); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return nil, ErrSupervisorShuttingDown
	}
	if _, exists := s.sessions[cfg.SessionID]; exists {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrSessionAlreadyExists, cfg.SessionID)
	}
	projectCount := 0
	globalCount := 0
	for _, ms := range s.sessions {
		if ms.info.State == SessionStateRunning || ms.info.State == SessionStateStarting || ms.info.State == SessionStateAttached {
			globalCount++
			if ms.info.ProjectID == cfg.ProjectID {
				projectCount++
			}
		}
	}
	if err := s.policy.CheckSessionLimits(projectCount, globalCount); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	if s.repoSetup != nil {
		env, err := s.repoSetup.Prepare(ctx, cfg.RepoPath, cfg.Env)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrRepoSetupFailed, trimSetupError(err))
		}
		cfg.Env = env
	}

	provider, err := s.resolveProvider(ctx, cfg.Options["provider"], cfg.Fallbacks, cfg.Env)
	if err != nil {
		return nil, err
	}

	if cfg.InitialCols == 0 {
		cfg.InitialCols = 120
	}
	if cfg.InitialRows == 0 {
		cfg.InitialRows = 40
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	cmd, err := provider.BuildCommand(sessionCtx, cfg)
	if err != nil {
		cancel()
		return nil, err
	}

	// Detect whether the provider requests stream-JSON mode (no PTY).
	useStreamJSON := false
	if sjp, ok := provider.(StreamJSONProvider); ok && sjp.IsStreamJSON() {
		useStreamJSON = true
	}

	// Detect whether the provider requests ANSI escape code stripping.
	stripANSI := false
	if sap, ok := provider.(StripANSIProvider); ok && sap.IsStripANSI() {
		stripANSI = true
	}

	now := nowUTC()
	ms := &managedSession{
		info: SessionInfo{
			SessionID: cfg.SessionID,
			ProjectID: cfg.ProjectID,
			Provider:  provider.ID(),
			RepoPath:  cfg.RepoPath,
			State:     SessionStateRunning,
			CreatedAt: now,
			Cols:      cfg.InitialCols,
			Rows:      cfg.InitialRows,
		},
		provider:     provider,
		cmd:          cmd,
		streamJSON:   useStreamJSON,
		stripANSI:    stripANSI,
		buf:          NewByteBuffer(s.bufSize),
		cancel:       cancel,
		stopGrace:    provider.StopGrace(),
		lastActivity: time.Now(),
	}

	if useStreamJSON {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		} else {
			cmd.SysProcAttr.Setpgid = true
		}
		stdinPipe, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			return nil, fmt.Errorf("get stdin pipe: %w", err)
		}
		// Use os.Pipe directly so that cmd.Wait does not close the read end via
		// closeAfterWait. readLoopStreamJSON owns the read end and receives a
		// natural EOF when the child process exits and the write end closes.
		stdoutR, stdoutW, err := os.Pipe()
		if err != nil {
			cancel()
			_ = stdinPipe.Close()
			return nil, fmt.Errorf("create stdout pipe: %w", err)
		}
		cmd.Stdout = stdoutW
		if err := cmd.Start(); err != nil {
			cancel()
			_ = stdinPipe.Close()
			_ = stdoutR.Close()
			_ = stdoutW.Close()
			return nil, fmt.Errorf("start stream-json session: %w", err)
		}
		// Close the write end in the parent; only the child holds it now.
		_ = stdoutW.Close()
		stdoutPipe := stdoutR
		ms.stdin = stdinPipe
		ms.info.ProcessID = cmd.Process.Pid
		s.mu.Lock()
		if s.shuttingDown {
			s.mu.Unlock()
			cancel()
			_ = stdinPipe.Close()
			_ = stdoutPipe.Close()
			_ = cmd.Wait()
			return nil, ErrSupervisorShuttingDown
		}
		if _, exists := s.sessions[cfg.SessionID]; exists {
			s.mu.Unlock()
			cancel()
			_ = stdinPipe.Close()
			return nil, fmt.Errorf("%w: %q", ErrSessionAlreadyExists, cfg.SessionID)
		}
		s.sessions[cfg.SessionID] = ms
		s.mu.Unlock()
		s.observeSessionStarted(ms)
		go s.readLoopStreamJSON(ms, stdoutPipe)
		go s.waitLoop(ms)
	} else {
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
			Cols: uint16(cfg.InitialCols),
			Rows: uint16(cfg.InitialRows),
		})
		if err != nil {
			cancel()
			return nil, fmt.Errorf("start pty session: %w", err)
		}
		ms.ptmx = ptmx
		ms.info.ProcessID = cmd.Process.Pid
		s.mu.Lock()
		if s.shuttingDown {
			s.mu.Unlock()
			cancel()
			_ = ptmx.Close()
			_ = cmd.Wait()
			return nil, ErrSupervisorShuttingDown
		}
		if _, exists := s.sessions[cfg.SessionID]; exists {
			s.mu.Unlock()
			cancel()
			_ = ptmx.Close()
			return nil, fmt.Errorf("%w: %q", ErrSessionAlreadyExists, cfg.SessionID)
		}
		s.sessions[cfg.SessionID] = ms
		s.mu.Unlock()
		s.observeSessionStarted(ms)
		go s.readLoop(ms)
		go s.waitLoop(ms)
	}

	info := ms.snapshotInfo()
	s.persistSession(info)
	return &info, nil
}

func (s *Supervisor) readLoop(ms *managedSession) {
	defer s.closeLive(ms)
	buf := make([]byte, 8192)
	for {
		n, err := ms.ptmx.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if ms.stripANSI {
				chunk = ansiEscape.ReplaceAll(chunk, nil)
			}
			slog.Debug("provider output", "session_id", ms.info.SessionID, "provider", ms.info.Provider, "bytes", len(chunk))
			s.appendChunk(ms, chunk, ChunkTypeOutput)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				slog.Info("session PTY closed", "session_id", ms.info.SessionID, "provider", ms.info.Provider)
			} else {
				ms.mu.Lock()
				stopping := ms.info.State == SessionStateStopping
				if ms.info.Error == "" && !ms.info.ExitRecorded && !stopping {
					ms.info.Error = err.Error()
				}
				ms.mu.Unlock()
				if stopping {
					slog.Debug("session PTY closed during stop", "session_id", ms.info.SessionID, "provider", ms.info.Provider, "error", err)
					return
				}
				slog.Warn("session PTY read error", "session_id", ms.info.SessionID, "provider", ms.info.Provider, "error", err)
				// Force-terminate the process so any pending WriteInput calls see
				// SessionNotFound rather than writing to a dead PTY file descriptor.
				sessionID := ms.info.SessionID
				go func() {
					if stopErr := s.Stop(sessionID, true); stopErr != nil {
						slog.Debug("stop after PTY read error", "session_id", sessionID, "error", stopErr)
					}
				}()
			}
			return
		}
	}
}

// claudeStreamEvent is the JSON shape emitted by `claude --output-format stream-json`.
// Only the fields we inspect are declared; unknown fields are discarded.
type claudeStreamEvent struct {
	Type  string `json:"type"`
	Delta *struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		Thinking string `json:"thinking,omitempty"`
	} `json:"delta,omitempty"`
}

// readLoopStreamJSON reads newline-delimited JSON from a stream-JSON provider's
// stdout, parses thinking and text deltas, and appends typed OutputChunks.
func (s *Supervisor) readLoopStreamJSON(ms *managedSession, r io.ReadCloser) {
	defer func() { _ = r.Close() }()
	defer s.closeLive(ms)
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			slog.Info("session stream-JSON pipe closed", "session_id", ms.info.SessionID, "provider", ms.info.Provider)
			return
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			if err != nil {
				// EOF or pipe closed by cmd.Wait — either way, no more data.
				slog.Info("session stream-JSON pipe closed", "session_id", ms.info.SessionID, "provider", ms.info.Provider)
				return
			}
			continue
		}
		var ev claudeStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Non-JSON line (e.g. a log or warning): emit as raw output.
			s.appendChunk(ms, line, ChunkTypeOutput)
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta != nil {
			switch ev.Delta.Type {
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					s.appendChunk(ms, []byte(ev.Delta.Thinking), ChunkTypeThinking)
				}
			case "text_delta":
				if ev.Delta.Text != "" {
					s.appendChunk(ms, []byte(ev.Delta.Text), ChunkTypeOutput)
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Warn("session stream-JSON read error", "session_id", ms.info.SessionID, "provider", ms.info.Provider, "error", err)
				ms.mu.Lock()
				if ms.info.Error == "" && !ms.info.ExitRecorded {
					ms.info.Error = err.Error()
				}
				ms.mu.Unlock()
			} else {
				slog.Info("session stream-JSON pipe closed", "session_id", ms.info.SessionID, "provider", ms.info.Provider)
			}
			return
		}
	}
}

// closeLive marks the session output as exhausted and closes every observer
// channel. Must only be called from readLoop or readLoopStreamJSON — after all
// sends to observer channels are complete.
// The observers map is kept intact so deferred Detach calls (from AttachSession
// goroutines draining their channels) can still clean up session state.
func (s *Supervisor) closeLive(ms *managedSession) {
	ms.mu.Lock()
	ms.liveClosed = true
	obs := make(map[string]*observerEntry, len(ms.observers))
	maps.Copy(obs, ms.observers)
	ms.mu.Unlock()
	for _, entry := range obs {
		close(entry.ch)
	}
}

// appendChunk adds a new chunk with the given type to the session buffer and
// fans it out to all attached observers. Chunks for slow observers are dropped
// with a warning; the observer remains attached.
//
// Sends are done under ms.mu with a non-blocking select so that closeLive
// (which also holds ms.mu when closing channels) cannot race.
func (s *Supervisor) appendChunk(ms *managedSession, payload []byte, ctype ChunkType) {
	if s.telemetry != nil {
		switch ctype {
		case ChunkTypeOutput:
			s.telemetry.ObserveProviderChunk(telemetrySession(ms), telemetry.StreamOutput, bytes.Clone(payload))
		case ChunkTypeThinking:
			s.telemetry.ObserveProviderChunk(telemetrySession(ms), telemetry.StreamThinking, bytes.Clone(payload))
		}
	}
	chunk := ms.buf.AppendTyped(payload, ctype)
	s.persistChunk(ms.info.SessionID, chunk)
	ms.mu.Lock()
	ms.info.OldestSeq = ms.buf.OldestSeq()
	ms.info.LastSeq = ms.buf.LastSeq()
	ms.lastActivity = time.Now()
	for clientID, entry := range ms.observers {
		select {
		case entry.ch <- chunk:
		default:
			slog.Warn("observer channel full, dropping chunk", "session_id", ms.info.SessionID, "client_id", clientID)
		}
	}
	ms.mu.Unlock()
}

// fanoutControlEvent broadcasts a control chunk to all current observers
// without appending it to the replay buffer or persisting it.
//
// Sends are done under ms.mu with a non-blocking select so that closeLive
// (which also holds ms.mu when closing channels) cannot race.
func (s *Supervisor) fanoutControlEvent(ms *managedSession, ctype ChunkType, payload []byte) {
	chunk := OutputChunk{Type: ctype, Payload: payload}
	ms.mu.Lock()
	if ms.liveClosed {
		ms.mu.Unlock()
		return
	}
	for clientID, entry := range ms.observers {
		select {
		case entry.ch <- chunk:
		default:
			slog.Warn("observer channel full, dropping control event", "session_id", ms.info.SessionID, "client_id", clientID, "type", ctype)
		}
	}
	ms.mu.Unlock()
}

// NotifyWriterClaimed broadcasts a ChunkTypeWriterClaimed control event to all
// observers of sessionID so they learn immediately which client now owns the
// writer role. The payload is the claimant clientID.
func (s *Supervisor) NotifyWriterClaimed(sessionID, claimantClientID string) {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	s.fanoutControlEvent(ms, ChunkTypeWriterClaimed, []byte(claimantClientID))
}

// NotifyWriterReleased broadcasts a ChunkTypeWriterReleased control event to
// all observers of sessionID. The payload is the releasing clientID.
func (s *Supervisor) NotifyWriterReleased(sessionID, releasingClientID string) {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	s.fanoutControlEvent(ms, ChunkTypeWriterReleased, []byte(releasingClientID))
}

func (s *Supervisor) waitLoop(ms *managedSession) {
	err := ms.cmd.Wait()

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	ms.mu.Lock()
	stopping := ms.info.State == SessionStateStopping
	ms.info.StoppedAt = nowUTC()
	ms.info.ExitRecorded = true
	ms.info.ExitCode = exitCode
	ms.info.ProcessID = 0
	if err != nil && !ms.forceStop && !stopping {
		ms.info.State = SessionStateFailed
		if ms.info.Error == "" {
			ms.info.Error = err.Error()
		}
		slog.Warn("session process failed", "session_id", ms.info.SessionID, "provider", ms.info.Provider, "exit_code", exitCode, "error", err)
	} else {
		ms.info.State = SessionStateStopped
		slog.Info("session process exited", "session_id", ms.info.SessionID, "provider", ms.info.Provider, "exit_code", exitCode)
	}
	ms.cancel()
	ms.mu.Unlock()
	if s.telemetry != nil {
		s.telemetry.SessionEnded(telemetrySession(ms))
	}

	s.persistSession(ms.snapshotInfo())
}

func (s *Supervisor) Stop(sessionID string, force bool) error {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}

	ms.mu.Lock()
	if ms.info.State == SessionStateStopped || ms.info.State == SessionStateFailed {
		slog.Debug("stop called on already-terminated session", "session_id", sessionID, "state", ms.info.State)
		ms.mu.Unlock()
		return nil
	}
	slog.Info("stopping session process", "session_id", sessionID, "provider", ms.info.Provider, "force", force, "pid", ms.info.ProcessID)
	if ms.recovered {
		ms.info.State = SessionStateStopping
		ms.forceStop = force
		pid := ms.info.ProcessID
		grace := ms.stopGrace
		ms.mu.Unlock()

		if force {
			if pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		} else if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}

		go func() {
			deadline := time.Now().Add(grace)
			for time.Now().Before(deadline) {
				if !processAlive(pid) {
					ms.mu.Lock()
					ms.info.State = SessionStateStopped
					ms.info.StoppedAt = nowUTC()
					ms.info.ProcessID = 0
					ms.mu.Unlock()
					s.persistSession(ms.snapshotInfo())
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			if !force && pid > 0 && processAlive(pid) {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			ms.mu.Lock()
			ms.info.State = SessionStateStopped
			ms.info.StoppedAt = nowUTC()
			ms.info.ProcessID = 0
			ms.mu.Unlock()
			s.persistSession(ms.snapshotInfo())
		}()
		return nil
	}
	ms.info.State = SessionStateStopping
	ms.forceStop = force
	pid := ms.cmd.Process.Pid
	grace := ms.stopGrace
	stdin := ms.stdin
	ms.mu.Unlock()

	// Closing stdin signals EOF to stream-JSON providers that read from stdin.
	if stdin != nil {
		_ = stdin.Close()
	}

	if force {
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		return nil
	}
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}

	go func() {
		time.Sleep(grace)
		ms.mu.Lock()
		state := ms.info.State
		pid := ms.cmd.Process.Pid
		ms.mu.Unlock()
		if state == SessionStateStopping && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
	return nil
}

func (s *Supervisor) WriteInput(sessionID, clientID string, data []byte) (int, error) {
	if err := s.policy.ValidateInputBytes(data); err != nil {
		return 0, err
	}
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	ms.mu.Lock()
	if ms.recovered {
		ms.mu.Unlock()
		return 0, ErrSessionRecoveryUnavailable
	}
	if ms.info.ActiveWriterClientID == "" {
		ms.mu.Unlock()
		return 0, ErrClientNotAttached
	}
	if ms.info.ActiveWriterClientID != clientID {
		ms.mu.Unlock()
		return 0, ErrClientMismatch
	}
	ms.lastActivity = time.Now()
	streamJSON := ms.streamJSON
	stdin := ms.stdin
	ptmx := ms.ptmx
	tsession := telemetry.Session{SessionID: ms.info.SessionID, ProjectID: ms.info.ProjectID, Provider: ms.info.Provider}
	ms.mu.Unlock()
	if s.telemetry != nil {
		s.telemetry.ObserveInputChunk(tsession, bytes.Clone(data))
	}
	slog.Debug("provider input", "session_id", sessionID, "provider", ms.info.Provider, "bytes", len(data))
	if streamJSON {
		n, err := stdin.Write(data)
		return n, err
	}
	n, err := ptmx.Write(data)
	return n, err
}

func (s *Supervisor) Resize(sessionID, clientID string, cols, rows uint32) error {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	ms.mu.Lock()
	if ms.recovered {
		ms.mu.Unlock()
		return ErrSessionRecoveryUnavailable
	}
	if ms.info.ActiveWriterClientID == "" {
		ms.mu.Unlock()
		return ErrClientNotAttached
	}
	if ms.info.ActiveWriterClientID != clientID {
		ms.mu.Unlock()
		return ErrClientMismatch
	}
	ms.info.Cols = cols
	ms.info.Rows = rows
	ms.lastActivity = time.Now()
	streamJSON := ms.streamJSON
	ptmx := ms.ptmx
	ms.mu.Unlock()
	if streamJSON {
		return nil // no PTY to resize for stream-JSON sessions
	}
	return pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Attach connects clientID to the session. role controls whether the client
// attaches as a writer (can send input) or observer (read-only).
//
// Writers: only one writer is allowed at a time. Returns ErrWriterConflict if
// another client already holds the writer slot.
//
// Observers: unlimited. Observers always succeed unless the session is not found.
func (s *Supervisor) Attach(sessionID, clientID string, afterSeq uint64, role AttachRole) (*AttachState, error) {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		// For stopped/failed sessions that were persisted in a previous daemon
		// lifetime, serve the stored chunks in read-only mode (no live channel).
		if state, err := s.attachHistory(sessionID, clientID, afterSeq); state != nil || err != ErrSessionNotFound {
			return state, err
		}
		return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.recovered {
		oldest := ms.buf.OldestSeq()
		last := ms.buf.LastSeq()
		closed := make(chan OutputChunk)
		close(closed)
		return &AttachState{
			ClientID:     clientID,
			Role:         AttachRoleObserver,
			Replay:       ms.buf.After(afterSeq),
			Live:         closed,
			ReplayGap:    oldest > 0 && afterSeq > 0 && afterSeq < oldest-1,
			OldestSeq:    oldest,
			LastSeq:      last,
			ExitRecorded: ms.info.ExitRecorded,
			ExitCode:     ms.info.ExitCode,
			Cols:         ms.info.Cols,
			Rows:         ms.info.Rows,
		}, nil
	}

	// Enforce single-writer constraint.
	if role == AttachRoleWriter && ms.info.ActiveWriterClientID != "" {
		return nil, ErrWriterConflict
	}

	if ms.observers == nil {
		ms.observers = make(map[string]*observerEntry)
	}

	// Close and evict any stale channel from a prior attach with the same client_id
	// to avoid leaking goroutines that are draining the old channel.
	if existing, ok := ms.observers[clientID]; ok {
		close(existing.ch)
		delete(ms.observers, clientID)
	}

	// Build the live channel. If the read loop already finished, hand the caller
	// a pre-closed channel so it drains immediately.
	var liveCh chan OutputChunk
	if ms.liveClosed {
		liveCh = make(chan OutputChunk)
		close(liveCh)
	} else {
		liveCh = make(chan OutputChunk, 128)
		ms.observers[clientID] = &observerEntry{ch: liveCh, role: role}
	}

	if role == AttachRoleWriter {
		ms.info.ActiveWriterClientID = clientID
		ms.info.Attached = true
		ms.info.AttachedClientID = clientID
		ms.info.State = SessionStateAttached
	}
	ms.info.ObserverCount = s.countObservers(ms)
	ms.lastActivity = time.Now()

	oldest := ms.buf.OldestSeq()
	last := ms.buf.LastSeq()
	return &AttachState{
		ClientID:     clientID,
		Role:         role,
		Replay:       ms.buf.After(afterSeq),
		Live:         liveCh,
		ReplayGap:    oldest > 0 && afterSeq > 0 && afterSeq < oldest-1,
		OldestSeq:    oldest,
		LastSeq:      last,
		ExitRecorded: ms.info.ExitRecorded,
		ExitCode:     ms.info.ExitCode,
		Cols:         ms.info.Cols,
		Rows:         ms.info.Rows,
	}, nil
}

// countObservers returns the number of read-only observers in ms.observers.
// Must be called with ms.mu held.
func (s *Supervisor) countObservers(ms *managedSession) int {
	n := 0
	for _, entry := range ms.observers {
		if entry.role == AttachRoleObserver {
			n++
		}
	}
	return n
}

// Detach removes clientID from the session's observer set. It returns true if
// the detaching client held the active-writer slot (so the server can broadcast
// a WRITER_RELEASED event), and any error.
func (s *Supervisor) Detach(sessionID, clientID string) (wasWriter bool, err error) {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		// History sessions are served read-only; detach is a no-op.
		s.histMu.RLock()
		_, inHistory := s.history[sessionID]
		s.histMu.RUnlock()
		if inHistory {
			return false, nil
		}
		return false, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.recovered {
		return false, nil
	}
	if _, present := ms.observers[clientID]; !present {
		return false, ErrClientMismatch
	}
	delete(ms.observers, clientID)

	// If the detaching client held the writer slot, clear it.
	if ms.info.ActiveWriterClientID == clientID {
		ms.info.ActiveWriterClientID = ""
		ms.info.Attached = false
		ms.info.AttachedClientID = ""
		wasWriter = true
	}
	ms.info.ObserverCount = s.countObservers(ms)
	if len(ms.observers) == 0 && ms.info.State == SessionStateAttached {
		ms.info.State = SessionStateRunning
	}
	return wasWriter, nil
}

func (s *Supervisor) Get(sessionID string) (*SessionInfo, error) {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if ok {
		info := ms.snapshotInfo()
		return &info, nil
	}
	// Fall back to history (sessions persisted from a previous daemon lifetime).
	s.histMu.RLock()
	info, ok := s.history[sessionID]
	s.histMu.RUnlock()
	if ok {
		return &info, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
}

func (s *Supervisor) List(projectID string) []SessionInfo {
	// Snapshot live session IDs and their info under the live lock.
	s.mu.RLock()
	liveIDs := make(map[string]struct{}, len(s.sessions))
	out := make([]SessionInfo, 0, len(s.sessions))
	for id, ms := range s.sessions {
		liveIDs[id] = struct{}{}
		info := ms.snapshotInfo()
		if projectID != "" && info.ProjectID != projectID {
			continue
		}
		out = append(out, info)
	}
	s.mu.RUnlock()

	// Append historical sessions not present in the live map.
	s.histMu.RLock()
	for id, info := range s.history {
		if _, live := liveIDs[id]; live {
			continue
		}
		if projectID != "" && info.ProjectID != projectID {
			continue
		}
		out = append(out, info)
	}
	s.histMu.RUnlock()
	return out
}

// Shutdown gracefully stops all active sessions and waits until they reach a
// terminal state. If ctx expires, any remaining sessions are force-stopped and
// ctx.Err() is returned.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.closeOnce.Do(func() {
		close(s.done)
	})

	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()

	s.stopSessions(s.nonTerminalSessionIDs(), false)
	if err := s.waitForAllSessions(ctx); err != nil {
		s.stopSessions(s.nonTerminalSessionIDs(), true)
		s.waitBestEffort(2 * time.Second)
		s.closeTelemetryBestEffort()
		return err
	}
	flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	s.closeTelemetry(flushCtx)
	return nil
}

func (s *Supervisor) nonTerminalSessionIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.sessions))
	for id, ms := range s.sessions {
		info := ms.snapshotInfo()
		if info.State == SessionStateStopped || info.State == SessionStateFailed {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func (s *Supervisor) stopSessions(ids []string, force bool) {
	for _, id := range ids {
		if err := s.Stop(id, force); err != nil && !errors.Is(err, ErrSessionNotFound) {
			slog.Warn("session stop failed", "session_id", id, "force", force, "error", err)
		}
	}
}

func (s *Supervisor) waitBestEffort(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.waitForAllSessions(ctx); err != nil {
		slog.Warn("timed out waiting for forced sessions to persist terminal state", "error", err)
	}
}

func (s *Supervisor) waitForAllSessions(ctx context.Context) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if len(s.nonTerminalSessionIDs()) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
	})
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()

	s.mu.RLock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		_ = s.Stop(id, true)
	}
	s.waitBestEffort(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.closeTelemetry(ctx)
}

func (s *Supervisor) observeSessionStarted(ms *managedSession) {
	if s.telemetry != nil {
		s.telemetry.SessionStarted(telemetrySession(ms))
	}
}

func telemetrySession(ms *managedSession) telemetry.Session {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return telemetry.Session{SessionID: ms.info.SessionID, ProjectID: ms.info.ProjectID, Provider: ms.info.Provider}
}

func (s *Supervisor) closeTelemetry(ctx context.Context) {
	if s.telemetry == nil {
		return
	}
	s.telemetryCloseOnce.Do(func() {
		if err := s.telemetry.Close(ctx); err != nil {
			slog.Warn("telemetry flush failed", "error", err)
		}
	})
}

func (s *Supervisor) closeTelemetryBestEffort() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.closeTelemetry(ctx)
}

// ClaimWriterResult is returned by ClaimWriter.
type ClaimWriterResult struct {
	// PreviousWriterClientID is set when force evicted an existing writer.
	PreviousWriterClientID string
}

// ClaimWriter promotes clientID to the active-writer slot. If force is true
// and another client holds the slot, that client is evicted (its channel is
// not closed here; the server must send them a WRITER_RELEASED event). Returns
// ErrWriterConflict when force is false and the slot is taken.
func (s *Supervisor) ClaimWriter(sessionID, clientID string, force bool) (*ClaimWriterResult, error) {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.recovered {
		return nil, ErrSessionRecoveryUnavailable
	}

	// Idempotent: caller already holds the slot.
	if ms.info.ActiveWriterClientID == clientID {
		return &ClaimWriterResult{}, nil
	}

	prevWriter := ms.info.ActiveWriterClientID
	if prevWriter != "" && !force {
		return nil, ErrWriterConflict
	}

	// Ensure the claimant is already observing (has a live channel).
	if _, present := ms.observers[clientID]; !present {
		return nil, fmt.Errorf("%w: client %q must be attached before claiming writer", ErrClientNotAttached, clientID)
	}

	// Downgrade the previous writer to observer in the observers map (keep their channel).
	if prevEntry, prev := ms.observers[prevWriter]; prev {
		prevEntry.role = AttachRoleObserver
	}

	ms.observers[clientID].role = AttachRoleWriter
	ms.info.ActiveWriterClientID = clientID
	ms.info.Attached = true
	ms.info.AttachedClientID = clientID
	if ms.info.State == SessionStateRunning {
		ms.info.State = SessionStateAttached
	}
	ms.info.ObserverCount = s.countObservers(ms)
	return &ClaimWriterResult{PreviousWriterClientID: prevWriter}, nil
}

// ReleaseWriter demotes clientID from the active-writer slot to observer.
// Returns ErrClientMismatch if clientID does not currently hold the slot.
func (s *Supervisor) ReleaseWriter(sessionID, clientID string) error {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.recovered {
		return nil
	}
	if ms.info.ActiveWriterClientID != clientID {
		return ErrClientMismatch
	}

	// Downgrade to observer.
	if entry, present := ms.observers[clientID]; present {
		entry.role = AttachRoleObserver
	}
	ms.info.ActiveWriterClientID = ""
	ms.info.Attached = false
	ms.info.AttachedClientID = ""
	ms.info.ObserverCount = s.countObservers(ms)
	if len(ms.observers) == 0 && ms.info.State == SessionStateAttached {
		ms.info.State = SessionStateRunning
	}
	return nil
}

func (ms *managedSession) snapshotInfo() SessionInfo {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	info := ms.info
	info.OldestSeq = ms.buf.OldestSeq()
	info.LastSeq = ms.buf.LastSeq()
	return info
}
