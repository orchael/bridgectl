package bridgecontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	dialTimeout           = 10 * time.Second
	writeTimeout          = 10 * time.Second
	helloAckTimeout       = 10 * time.Second
	defaultHeartbeatEvery = 30 * time.Second
	eventQueueSize        = 64
	// shutdownFlushBudget bounds the best-effort attempt to send any
	// lifecycle notifications still queued when a graceful shutdown begins,
	// matching the 2s budgets Supervisor already gives telemetry/control
	// Close calls elsewhere.
	shutdownFlushBudget = 2 * time.Second
)

// minIdleReadTimeout is a var (not const) so tests can shrink it to keep a
// silent-connection detection test fast without changing production
// behavior.
var minIdleReadTimeout = 30 * time.Second

// Config configures a Client. Endpoint and Credential are required; a
// Client should simply not be constructed (or Started) when Bridge
// enrollment has no control endpoint/credential, per the "no Bridge
// configuration means no control connection is attempted" invariant.
type Config struct {
	// Endpoint is the control-plane WebSocket URL, e.g.
	// "wss://control.bridge.orchael.dev/v1/control". Taken verbatim from
	// Bridge's enrollment response; never hard-coded here.
	Endpoint string
	// Credential is the bri_... control-only credential. Never the
	// telemetry-only brc_ credential.
	Credential string
	// BridgectlVersion is sent in hello.bridgectl_version.
	BridgectlVersion string
	// InstallationID is used only for status reporting/logging.
	InstallationID string

	// StatusPath, if set, receives a small JSON status snapshot on every
	// connection-state change, for `bridgectl doctor` to read. Optional.
	StatusPath string
	// RevisionPath, if set, persists per-session revision counters so they
	// survive a daemon restart. Optional but strongly recommended in
	// production wiring (see RevisionStore's doc comment).
	RevisionPath string

	// SnapshotFunc returns the current authoritative list of active
	// sessions. Called once per successful (re)connect, immediately after
	// hello_ack. A nil result is treated as zero active sessions.
	SnapshotFunc func() []SessionSnapshot

	// HTTPClient overrides the client used for the WebSocket upgrade
	// request. Nil (the default) uses coder/websocket's own default
	// client/transport, which performs normal TLS certificate verification.
	// This exists for tests to point at a local TLS test server (e.g.
	// httptest.NewTLSServer's own .Client(), which trusts that server's
	// certificate) — production wiring must never set this to a client with
	// relaxed certificate verification.
	HTTPClient *http.Client

	Logger *slog.Logger
}

// Client is the optional outbound control-plane WebSocket client. All
// exported methods are non-blocking and safe to call from the Supervisor's
// hot path; the actual network I/O happens on a single background
// goroutine started by Start.
type Client struct {
	cfg       Config
	revisions *RevisionStore
	rng       *rand.Rand
	rngMu     sync.Mutex

	events    chan SessionSnapshot
	closeCh   chan struct{}
	closeOnce sync.Once
	doneCh    chan struct{}

	// everConnected is set after the first successful connection's initial
	// snapshot. It is only ever read/written from within connectAndServe,
	// which run's single goroutine calls sequentially (never concurrently),
	// so it needs no additional synchronization.
	everConnected bool
}

// New constructs a Client. It does not connect until Start is called.
func New(cfg Config) *Client {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{
		cfg:       cfg,
		revisions: NewRevisionStore(cfg.RevisionPath),
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
		events:    make(chan SessionSnapshot, eventQueueSize),
		closeCh:   make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start begins the background connect/reconnect loop. It returns
// immediately; ctx bounds the connection's lifetime in addition to Close.
func (c *Client) Start(ctx context.Context) {
	go c.run(ctx)
}

// Notify enqueues a session lifecycle change for publication to Bridge. It
// never blocks the caller (the Supervisor): if the bounded queue is full,
// the oldest pending notification is dropped to make room. A dropped
// notification never corrupts Bridge's view of the world because revisions
// are assigned at send time (see sendEvent) and the next successful
// snapshot/event always reflects bridgectl's then-current authoritative
// state.
func (c *Client) Notify(info SessionSnapshot) {
	select {
	case c.events <- info:
		return
	default:
	}
	select {
	case <-c.events:
	default:
	}
	select {
	case c.events <- info:
	default:
		c.cfg.Logger.Warn("bridgecontrol: event queue full, dropped notification", "session_id", info.SessionID)
	}
}

// Close stops the client and waits for its background goroutine to exit, or
// for ctx to expire. It is safe to call multiple times.
func (c *Client) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { close(c.closeCh) })
	select {
	case <-c.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) run(ctx context.Context) {
	defer close(c.doneCh)
	defer c.setStatus(StateDisconnected, "")

	attempt := 0
	for {
		select {
		case <-c.closeCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		c.setStatus(StateConnecting, "")
		err := c.connectAndServe(ctx)
		if err == nil {
			return // graceful local shutdown
		}
		attempt++
		c.setStatus(classifyStatus(err), err.Error())
		delay := c.nextBackoff(attempt)
		c.cfg.Logger.Warn("bridgecontrol: connection lost, reconnecting", "error", err, "attempt", attempt, "delay", delay)
		select {
		case <-time.After(delay):
		case <-c.closeCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) nextBackoff(attempt int) time.Duration {
	c.rngMu.Lock()
	defer c.rngMu.Unlock()
	return backoffDelay(attempt, c.rng)
}

func (c *Client) setStatus(state State, lastErr string) {
	if c.cfg.StatusPath == "" {
		return
	}
	st := Status{State: state, InstallationID: c.cfg.InstallationID, LastError: lastErr, UpdatedAt: time.Now().UTC()}
	if err := WriteStatus(c.cfg.StatusPath, st); err != nil {
		c.cfg.Logger.Warn("bridgecontrol: failed to persist status", "error", err)
	}
}

// classifyStatus turns a connection failure into a coarse status for
// `doctor`. Bridge's control protocol has no dedicated error code
// distinguishing "credential rejected" from other closes (see
// docs/control-plane-v1.md); this is a best-effort classification based on
// the documented HTTP status codes and close reason text.
func classifyStatus(err error) State {
	if err == nil {
		return StateDisconnected
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "401"), strings.Contains(msg, "invalid control credential"), strings.Contains(msg, "credential no longer valid"):
		return StateAuthRejected
	case strings.Contains(msg, "503"), strings.Contains(msg, "too many connections"):
		return StateUnavailable
	default:
		return StateDisconnected
	}
}

func (c *Client) connectAndServe(parent context.Context) error {
	dialCtx, cancel := context.WithTimeout(parent, dialTimeout)
	defer cancel()

	conn, resp, err := websocket.Dial(dialCtx, c.cfg.Endpoint, &websocket.DialOptions{
		HTTPClient: c.cfg.HTTPClient,
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.cfg.Credential}},
	})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial control endpoint: HTTP %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("dial control endpoint: %w", err)
	}
	conn.SetReadLimit(maxMessageBytes)
	defer func() { _ = conn.CloseNow() }()

	if err := c.sendHello(parent, conn); err != nil {
		return err
	}
	ack, err := c.readHelloAck(parent, conn)
	if err != nil {
		return err
	}
	c.setStatus(StateConnected, "")
	c.cfg.Logger.Info("bridgecontrol: connected", "installation_id", ack.InstallationID, "heartbeat_interval_s", ack.HeartbeatIntervalSeconds)

	if err := c.sendSnapshot(parent, conn); err != nil {
		return err
	}
	if c.everConnected {
		// This is a reconnect, not the first-ever connection: anything
		// still queued can only have been enqueued while disconnected (a
		// live connection drains c.events as fast as it fills), so it
		// necessarily predates the live Supervisor state this snapshot just
		// captured. Applying it now with a freshly assigned revision would
		// let that stale, pre-snapshot state overwrite what the snapshot
		// just established, and would replay rather than reconcile. This
		// reasoning does not hold for the very first connection (nothing to
		// have gone stale relative to yet), so skip the drain there: a
		// notification racing the first handshake is still fresh, and
		// discarding it would be a real, avoidable loss of the only report
		// of that state change.
		c.drainEvents()
	}
	c.everConnected = true

	heartbeatInterval := time.Duration(ack.HeartbeatIntervalSeconds) * time.Second
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultHeartbeatEvery
	}
	return c.serve(parent, conn, heartbeatInterval)
}

func (c *Client) serve(ctx context.Context, conn *websocket.Conn, heartbeatInterval time.Duration) error {
	readErrCh := make(chan error, 1)
	reconcileCh := make(chan struct{}, 1)
	// idleReadTimeout detects a silently dead connection (e.g. a network
	// partition or a docker/NAT path black-holing packets) that never
	// produces an explicit read/write error: a TCP write can keep
	// "succeeding" locally (accepted into the kernel send buffer) long after
	// delivery has actually stopped, so relying on write errors alone can
	// leave a dead connection undetected for a very long time. Bounding
	// every read to a few heartbeat intervals means bridgectl notices within
	// a bounded window even when the peer never sends a close frame, mirroring
	// Bridge's own 90s server-side idle timeout for a 30s heartbeat interval.
	idleReadTimeout := 3 * heartbeatInterval
	if idleReadTimeout < minIdleReadTimeout {
		idleReadTimeout = minIdleReadTimeout
	}
	go c.readLoop(ctx, conn, idleReadTimeout, readErrCh, reconcileCh)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeCh:
			// Supervisor.Shutdown/Close already enqueues final terminal
			// notifications (e.g. session_stopped) before calling
			// Client.Close; without a best-effort flush here they would sit
			// in the queue forever once the socket closes, leaving Bridge's
			// view of those sessions stale until a reconnect that may never
			// happen.
			c.flushPendingEvents(conn)
			_ = conn.Close(websocket.StatusNormalClosure, "client shutting down")
			return nil
		case <-ctx.Done():
			c.flushPendingEvents(conn)
			_ = conn.Close(websocket.StatusNormalClosure, "shutting down")
			return nil
		case err := <-readErrCh:
			return err
		case <-reconcileCh:
			if err := c.sendSnapshot(ctx, conn); err != nil {
				return err
			}
			c.drainEvents() // see the identical comment in connectAndServe
		case <-ticker.C:
			if err := c.sendHeartbeat(ctx, conn); err != nil {
				return err
			}
		case info := <-c.events:
			if err := c.sendEvent(ctx, conn, info); err != nil {
				return err
			}
		}
	}
}

// readLoop owns the connection's only Read call, per coder/websocket's
// single-reader requirement. It runs concurrently with serve's writes
// (concurrent Read+Write from separate goroutines is explicitly supported).
// Each read is individually bounded by idleTimeout so a peer that stops
// responding entirely (rather than closing cleanly) is still detected.
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn, idleTimeout time.Duration, errCh chan<- error, reconcileCh chan<- struct{}) {
	for {
		rctx, cancel := context.WithTimeout(ctx, idleTimeout)
		_, data, err := conn.Read(rctx)
		cancel()
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			select {
			case errCh <- fmt.Errorf("malformed server message: %w", err):
			default:
			}
			return
		}
		switch env.Type {
		case msgError:
			var p errorPayload
			_ = json.Unmarshal(env.Payload, &p)
			c.cfg.Logger.Warn("bridgecontrol: server error", "code", p.Code, "message", p.Message)
			if p.ReconcileRequired || p.Code == errCodeRevisionGap {
				select {
				case reconcileCh <- struct{}{}:
				default:
				}
			}
		case msgAck:
			// "applied" and "duplicate" both indicate success; nothing to do.
		default:
			// Bridge's milestone-1 protocol never pushes any other message
			// type; ignore unknown types rather than treating them as fatal,
			// in case a future protocol version adds informational messages.
		}
	}
}

// drainEvents discards any notifications already sitting in the queue.
// Called immediately after an authoritative snapshot is sent (initial
// connect, and revision_gap reconciliation): anything still queued predates
// that snapshot's data-gathering, so sending it afterward would risk
// overwriting the snapshot's just-established state with stale data. A
// notification that happens to be enqueued in the narrow window between
// the snapshot's SnapshotFunc() call and this drain is lost, same as any
// other dropped notification (see Notify's doc comment) — the next real
// state change on that session produces a fresh one.
func (c *Client) drainEvents() {
	for {
		select {
		case <-c.events:
		default:
			return
		}
	}
}

// flushPendingEvents makes a best-effort, time-bounded attempt to send any
// notifications still queued when a graceful shutdown begins, so a final
// session_stopped enqueued just before Close() isn't silently lost. Unlike
// drainEvents, this path sends (rather than discards) the queue, because
// shutdown is the one case where the client is not about to reconcile with
// a fresh snapshot afterward.
//
// Deliberately uses a fresh background deadline rather than the caller's
// ctx: this is invoked from the very branches (closeCh/ctx.Done) where the
// caller's context may already be cancelled or expired, which would make
// every send fail immediately and defeat the flush entirely.
func (c *Client) flushPendingEvents(conn *websocket.Conn) {
	deadline := time.Now().Add(shutdownFlushBudget)
	for time.Now().Before(deadline) {
		var info SessionSnapshot
		select {
		case info = <-c.events:
		default:
			return
		}
		sendCtx, cancel := context.WithDeadline(context.Background(), deadline)
		err := c.sendEvent(sendCtx, conn, info)
		cancel()
		if err != nil {
			return // connection is going or gone; nothing more we can do
		}
	}
}

func (c *Client) writeEnvelope(ctx context.Context, conn *websocket.Conn, msgType string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = b
	}
	env := envelope{ProtocolVersion: ProtocolVersion, Type: msgType, MessageID: uuid.NewString(), Payload: raw}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func (c *Client) sendHello(ctx context.Context, conn *websocket.Conn) error {
	version := c.cfg.BridgectlVersion
	if version == "" {
		version = "dev"
	}
	if len(version) > maxBridgectlVersion {
		version = version[:maxBridgectlVersion]
	}
	return c.writeEnvelope(ctx, conn, msgHello, helloPayload{BridgectlVersion: version})
}

func (c *Client) readHelloAck(ctx context.Context, conn *websocket.Conn) (*helloAckPayload, error) {
	rctx, cancel := context.WithTimeout(ctx, helloAckTimeout)
	defer cancel()
	_, data, err := conn.Read(rctx)
	if err != nil {
		return nil, fmt.Errorf("read hello_ack: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("malformed hello_ack: %w", err)
	}
	if env.Type == msgError {
		var p errorPayload
		_ = json.Unmarshal(env.Payload, &p)
		return nil, fmt.Errorf("bridge rejected hello: %s: %s", p.Code, p.Message)
	}
	if env.Type != msgHelloAck {
		return nil, fmt.Errorf("unexpected message %q before hello_ack", env.Type)
	}
	var ack helloAckPayload
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		return nil, fmt.Errorf("malformed hello_ack payload: %w", err)
	}
	return &ack, nil
}

func (c *Client) sendSnapshot(ctx context.Context, conn *websocket.Conn) error {
	var snaps []SessionSnapshot
	if c.cfg.SnapshotFunc != nil {
		snaps = c.cfg.SnapshotFunc()
	}
	if len(snaps) > maxSnapshotSessions {
		snaps = snaps[:maxSnapshotSessions]
		c.cfg.Logger.Warn("bridgecontrol: active session count exceeds snapshot limit, truncating", "limit", maxSnapshotSessions)
	}
	sessions := make([]sessionPayload, 0, len(snaps))
	now := time.Now().UTC()
	for _, s := range snaps {
		rev := c.revisions.Current(s.SessionID)
		if rev <= 0 {
			rev = c.revisions.Next(s.SessionID)
		}
		occurred := s.CreatedAt.UTC()
		if occurred.IsZero() {
			occurred = now
		}
		sessions = append(sessions, sessionPayload{
			SessionID:  s.SessionID,
			Provider:   s.Provider,
			ProjectID:  s.ProjectID,
			Status:     s.Status,
			Revision:   rev,
			OccurredAt: occurred,
		})
	}
	return c.writeEnvelope(ctx, conn, msgSessionSnapshot, sessionSnapshotPayload{Sessions: sessions})
}

func (c *Client) sendHeartbeat(ctx context.Context, conn *websocket.Conn) error {
	if err := c.writeEnvelope(ctx, conn, msgHeartbeat, nil); err != nil {
		return err
	}
	// The status file is otherwise written only on state *change*, so a
	// healthy long-lived connection would leave a "connected" entry
	// untouched for as long as it stays up. Refreshing it here is what
	// makes doctor's staleness check (bridgecontrol.StatusStaleAfter)
	// meaningful: an old timestamp can then only mean heartbeats have
	// actually stopped (daemon killed, powered off, or otherwise wedged),
	// not "still connected, just hasn't needed to write again."
	c.setStatus(StateConnected, "")
	return nil
}

// sendEvent publishes one incremental lifecycle change. It classifies the
// message type from local bookkeeping (has bridgectl ever told Bridge about
// this session id before, and is the new status terminal), not from the
// Supervisor call site, since a dropped-and-later-resent notification must
// still map to a coherent session_started/updated/stopped sequence.
func (c *Client) sendEvent(ctx context.Context, conn *websocket.Conn, info SessionSnapshot) error {
	existed := c.revisions.Current(info.SessionID) > 0
	rev := c.revisions.Next(info.SessionID)
	msgType := msgSessionUpdated
	switch {
	case !existed:
		msgType = msgSessionStarted
	case info.Status == StatusStopped || info.Status == StatusFailed:
		msgType = msgSessionStopped
	}
	payload := sessionPayload{
		SessionID:  info.SessionID,
		Provider:   info.Provider,
		ProjectID:  info.ProjectID,
		Status:     info.Status,
		Revision:   rev,
		OccurredAt: time.Now().UTC(),
	}
	if err := c.writeEnvelope(ctx, conn, msgType, payload); err != nil {
		return err
	}
	if msgType == msgSessionStopped {
		c.revisions.Forget(info.SessionID)
	}
	return nil
}
