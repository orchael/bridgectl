package bridgecontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestClient(t *testing.T, endpoint, credential string, snapshotFn func() []SessionSnapshot) *Client {
	t.Helper()
	dir := t.TempDir()
	return New(Config{
		Endpoint:         endpoint,
		Credential:       credential,
		BridgectlVersion: "test-version",
		InstallationID:   "install-1",
		StatusPath:       dir + "/status.json",
		RevisionPath:     dir + "/revisions.json",
		SnapshotFunc:     snapshotFn,
		Logger:           testLogger(),
	})
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// TestConnectAndServe_WssEndpoint_WithCustomHTTPClient exercises a real
// wss:// (TLS) connection end to end, using Config.HTTPClient to trust the
// test server's certificate — the supported way to point the client at a
// local TLS test server (or a custom/self-hosted control endpoint whose CA
// isn't in the system trust store) without weakening default certificate
// verification for production use, where HTTPClient stays nil.
func TestConnectAndServe_WssEndpoint_WithCustomHTTPClient(t *testing.T) {
	var helloEnv envelope
	done := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		var handshakeErr error
		helloEnv, _, handshakeErr = serverHandshake(conn, 30)
		if handshakeErr != nil {
			t.Errorf("server handshake: %v", handshakeErr)
		}
		close(done)
	}))
	defer server.Close()

	wssURL := "wss" + strings.TrimPrefix(server.URL, "https")
	c := New(Config{
		Endpoint:         wssURL,
		Credential:       "bri_valid",
		BridgectlVersion: "test-version",
		HTTPClient:       server.Client(), // trusts this test server's cert
		SnapshotFunc:     func() []SessionSnapshot { return nil },
		Logger:           testLogger(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_ = c.connectAndServe(ctx)

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("server handler did not complete")
	}
	if helloEnv.Type != msgHello {
		t.Fatalf("expected hello over the TLS connection, got %q", helloEnv.Type)
	}
}

// --- valid credential / handshake / initial snapshot ---

func TestConnectAndServe_ValidCredential_HandshakeAndNonEmptySnapshot(t *testing.T) {
	var helloEnv, snapshotEnv envelope
	done := make(chan struct{})
	fs := newFakeServer(t, testCredential("bri_valid"), func(conn *websocket.Conn, _ int) {
		var err error
		helloEnv, snapshotEnv, err = serverHandshake(conn, 30)
		if err != nil {
			t.Errorf("server handshake: %v", err)
		}
		close(done)
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot {
		return []SessionSnapshot{{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusRunning, CreatedAt: time.Now()}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_ = c.connectAndServe(ctx)

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("server handler did not complete")
	}

	if helloEnv.Type != msgHello {
		t.Fatalf("expected hello, got %q", helloEnv.Type)
	}
	var hp helloPayload
	if err := json.Unmarshal(helloEnv.Payload, &hp); err != nil {
		t.Fatalf("decode hello payload: %v", err)
	}
	if hp.BridgectlVersion != "test-version" {
		t.Fatalf("bridgectl_version = %q, want test-version", hp.BridgectlVersion)
	}
	if snapshotEnv.Type != msgSessionSnapshot {
		t.Fatalf("expected session_snapshot, got %q", snapshotEnv.Type)
	}
	var sp sessionSnapshotPayload
	if err := json.Unmarshal(snapshotEnv.Payload, &sp); err != nil {
		t.Fatalf("decode snapshot payload: %v", err)
	}
	if len(sp.Sessions) != 1 || sp.Sessions[0].SessionID != "s1" || sp.Sessions[0].Revision != 1 {
		t.Fatalf("unexpected snapshot sessions: %+v", sp.Sessions)
	}
}

func TestConnectAndServe_EmptySnapshot_SendsExplicitEmptyArray(t *testing.T) {
	var snapshotEnv envelope
	done := make(chan struct{})
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		var err error
		_, snapshotEnv, err = serverHandshake(conn, 30)
		if err != nil {
			t.Errorf("server handshake: %v", err)
		}
		close(done)
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_ = c.connectAndServe(ctx)

	<-done
	if !strings.Contains(string(snapshotEnv.Payload), `"sessions":[]`) {
		t.Fatalf("expected explicit empty sessions array, got %s", snapshotEnv.Payload)
	}
}

// --- credential rejection ---

func TestConnectAndServe_InvalidCredential(t *testing.T) {
	fs := newFakeServer(t, testCredential("bri_correct"), func(_ *websocket.Conn, _ int) {
		t.Error("handler should not run for a rejected credential")
	})
	defer fs.Close()

	for _, cred := range []string{"bri_wrong", ""} {
		c := newTestClient(t, fs.wsURL(), cred, nil)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		err := c.connectAndServe(ctx)
		cancel()
		if err == nil {
			t.Fatalf("credential %q: expected error, got nil", cred)
		}
		if classifyStatus(err) != StateAuthRejected {
			t.Fatalf("credential %q: classifyStatus = %v, want StateAuthRejected (err=%v)", cred, classifyStatus(err), err)
		}
	}
}

// --- protocol version negotiation ---

func TestConnectAndServe_UnsupportedProtocolVersion(t *testing.T) {
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		hello, err := serverReadEnvelope(conn)
		if err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		_ = serverWriteEnvelope(conn, msgError, hello.MessageID, errorPayload{
			MessageID: hello.MessageID, Code: errCodeUnsupportedProtocol, Message: "unsupported protocol_version",
		})
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", nil)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	err := c.connectAndServe(ctx)
	if err == nil || !strings.Contains(err.Error(), errCodeUnsupportedProtocol) {
		t.Fatalf("expected unsupported_protocol error, got %v", err)
	}
}

// --- malformed / oversized server messages ---

func TestConnectAndServe_MalformedServerMessage(t *testing.T) {
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_ = conn.Write(ctx, websocket.MessageText, []byte("not json"))
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	err := c.connectAndServe(ctx)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("expected malformed server message error, got %v", err)
	}
}

func TestConnectAndServe_OversizedMessage(t *testing.T) {
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		huge := make([]byte, maxMessageBytes+4096)
		for i := range huge {
			huge[i] = 'a'
		}
		_ = conn.Write(ctx, websocket.MessageText, huge)
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	err := c.connectAndServe(ctx)
	if err == nil {
		t.Fatal("expected an error for an oversized server message")
	}
}

// --- session lifecycle: started / updated / stopped with monotonic revisions ---

func TestSessionLifecycle_StartedUpdatedStopped(t *testing.T) {
	snapshotSeen := make(chan struct{})
	var closeOnce sync.Once
	var fs *fakeServer
	fs = newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		closeOnce.Do(func() { close(snapshotSeen) })
		for {
			env, err := serverReadEnvelope(conn)
			if err != nil {
				return
			}
			fs.record(env)
			_ = serverWriteEnvelope(conn, msgAck, "srv-ack", ackPayload{MessageID: env.MessageID, Result: "applied"})
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() { _ = c.connectAndServe(ctx) }()

	// Wait until the server has already acked the initial snapshot (and the
	// client has drained anything predating it) before notifying: a real
	// Supervisor's SnapshotFunc always reflects the same live state a
	// concurrent Notify describes, so a notification racing the handshake
	// would already be captured by the snapshot itself and correctly
	// dropped as redundant. This fake server's SnapshotFunc is a static nil
	// stub, so it can't reflect that — notifying only after the snapshot is
	// what makes this test's events unambiguously post-snapshot.
	select {
	case <-snapshotSeen:
	case <-time.After(testTimeout):
		t.Fatal("server never received the initial snapshot")
	}
	// The server receiving the snapshot only proves the client sent it; give
	// the client's own drainStalePending (which runs immediately after,
	// before the server's ack can arrive) a brief moment to finish first.
	time.Sleep(50 * time.Millisecond)

	// Notify is called once per transition and each is given a moment to be
	// drained and sent before the next: Client.Notify coalesces repeated
	// updates for the *same* session_id into a single pending entry (by
	// design — only the latest state matters if updates arrive faster than
	// they can be sent), so notifying all three transitions back-to-back
	// would legitimately collapse into just the final "stopped" message
	// rather than three distinct ones. Spacing them out is what makes this
	// test about the started/updated/stopped sequence, not about coalescing
	// (which has its own dedicated test).
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusRunning, CreatedAt: time.Now()})
	waitFor(t, testTimeout, func() bool { return len(fs.recorded()) >= 1 })
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusAttached, CreatedAt: time.Now()})
	waitFor(t, testTimeout, func() bool { return len(fs.recorded()) >= 2 })
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusStopped, CreatedAt: time.Now()})
	waitFor(t, testTimeout, func() bool { return len(fs.recorded()) >= 3 })
	msgs := fs.recorded()
	if len(msgs) < 3 {
		t.Fatalf("expected at least 3 recorded messages, got %d", len(msgs))
	}
	wantTypes := []string{msgSessionStarted, msgSessionUpdated, msgSessionStopped}
	for i, want := range wantTypes {
		if msgs[i].Type != want {
			t.Fatalf("message %d: type = %q, want %q", i, msgs[i].Type, want)
		}
		var sp sessionPayload
		if err := json.Unmarshal(msgs[i].Payload, &sp); err != nil {
			t.Fatalf("decode message %d payload: %v", i, err)
		}
		if sp.Revision != int64(i+1) {
			t.Fatalf("message %d: revision = %d, want %d", i, sp.Revision, i+1)
		}
	}
}

// --- revision_gap triggers reconciliation via a fresh snapshot ---

func TestRevisionGap_TriggersReconciliationSnapshot(t *testing.T) {
	var snapshotCount int32
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
		atomic.AddInt32(&snapshotCount, 1)
		env, err := serverReadEnvelope(conn) // the session_started from Notify
		if err != nil {
			return
		}
		// Simulate Bridge detecting a gap: send a reconcile-required error
		// instead of an ack.
		_ = serverWriteEnvelope(conn, msgError, env.MessageID, errorPayload{
			MessageID: env.MessageID, Code: errCodeRevisionGap, Message: "revision gap detected", ReconcileRequired: true,
		})
		// The client must now send a fresh session_snapshot.
		next, err := serverReadEnvelope(conn)
		if err != nil {
			t.Errorf("read post-gap message: %v", err)
			return
		}
		if next.Type == msgSessionSnapshot {
			atomic.AddInt32(&snapshotCount, 1)
		}
		_ = serverWriteEnvelope(conn, msgAck, "srv-ack", ackPayload{MessageID: next.MessageID, Result: "applied"})
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() { _ = c.connectAndServe(ctx) }()

	waitFor(t, testTimeout, func() bool { return atomic.LoadInt32(&snapshotCount) >= 1 })
	c.Notify(SessionSnapshot{SessionID: "s1", Status: StatusRunning, CreatedAt: time.Now()})

	waitFor(t, testTimeout, func() bool { return atomic.LoadInt32(&snapshotCount) >= 2 })
}

// --- heartbeat ---

func TestHeartbeat_SentWithinInterval(t *testing.T) {
	heartbeatSeen := make(chan struct{}, 1)
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 1); err != nil { // 1s heartbeat interval
			t.Errorf("handshake: %v", err)
			return
		}
		for {
			env, err := serverReadEnvelope(conn)
			if err != nil {
				return
			}
			if env.Type == msgHeartbeat {
				_ = serverWriteEnvelope(conn, msgAck, "srv-ack", ackPayload{MessageID: env.MessageID, Result: "applied"})
				select {
				case heartbeatSeen <- struct{}{}:
				default:
				}
				return
			}
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() { _ = c.connectAndServe(ctx) }()

	select {
	case <-heartbeatSeen:
	case <-time.After(testTimeout):
		t.Fatal("no heartbeat received within timeout")
	}
}

// --- server disconnect / client reconnect / fresh snapshot after reconnect ---

func TestReconnect_AfterServerDisconnect_SendsFreshSnapshot(t *testing.T) {
	origBase, origMin, origMax := backoffBase, backoffMin, backoffMax
	backoffBase, backoffMin, backoffMax = 10*time.Millisecond, 5*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backoffBase, backoffMin, backoffMax = origBase, origMin, origMax })

	fs := newFakeServer(t, nil, func(conn *websocket.Conn, n int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
		if n == 1 {
			// Simulate a dropped connection right after the initial snapshot.
			_ = conn.CloseNow()
			return
		}
		// Second connection: block until the test is done.
		_, _ = serverReadEnvelope(conn)
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	c.Start(ctx)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), testTimeout)
		defer closeCancel()
		_ = c.Close(closeCtx)
	}()

	waitFor(t, testTimeout, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.connCount >= 2
	})
}

// TestReconnect_DiscardsPreSnapshotQueuedEvents guards against a queued
// notification from before a disconnect being replayed on top of the fresh
// authoritative snapshot sent after reconnecting: anything still queued at
// reconnect time was necessarily enqueued while disconnected (a live
// connection drains the queue as fast as it fills), so it predates the
// snapshot's live Supervisor read and must be discarded, not replayed with
// a newly assigned revision.
func TestReconnect_DiscardsPreSnapshotQueuedEvents(t *testing.T) {
	origBase, origMin, origMax := backoffBase, backoffMin, backoffMax
	backoffBase, backoffMin, backoffMax = 10*time.Millisecond, 5*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backoffBase, backoffMin, backoffMax = origBase, origMin, origMax })

	var mu sync.Mutex
	var secondConnMessages []envelope
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, n int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
		if n == 1 {
			_ = conn.CloseNow() // drop right after the first snapshot
			return
		}
		// Second connection (the reconnect): record everything the client
		// sends afterward; there must be no incremental event for the
		// notification that was queued while disconnected.
		for {
			env, err := serverReadEnvelope(conn)
			if err != nil {
				return
			}
			mu.Lock()
			secondConnMessages = append(secondConnMessages, env)
			mu.Unlock()
			_ = serverWriteEnvelope(conn, msgAck, "srv-ack", ackPayload{MessageID: env.MessageID, Result: "applied"})
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	c.Start(ctx)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), testTimeout)
		defer closeCancel()
		_ = c.Close(closeCtx)
	}()

	// Enqueue while the first connection is being torn down / before the
	// reconnect completes. This is the notification that must never reach
	// the second connection as a replayed incremental event.
	c.Notify(SessionSnapshot{SessionID: "stale-session", Status: StatusRunning, CreatedAt: time.Now()})

	waitFor(t, testTimeout, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.connCount >= 2
	})
	// Give the (would-be) replay a chance to happen before asserting it didn't.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, env := range secondConnMessages {
		if env.Type == msgSessionStarted || env.Type == msgSessionUpdated || env.Type == msgSessionStopped {
			var sp sessionPayload
			_ = json.Unmarshal(env.Payload, &sp)
			if sp.SessionID == "stale-session" {
				t.Fatalf("stale pre-reconnect notification for %q was replayed as %q on the new connection", sp.SessionID, env.Type)
			}
		}
	}
}

// TestReconnect_DiscardedTerminalNotificationForgetsRevision guards against
// a revisions-file leak: if a discarded stale notification describes a
// terminal (stopped/failed) session, sendEvent (and its RevisionStore.Forget
// call) never runs for it, since the notification is dropped rather than
// sent. Without an explicit Forget in the discard path itself, that
// session's entry would remain in the revisions map forever, growing the
// persisted file by one entry per session whose final notification happened
// to be discarded during a reconcile — silently contradicting the
// bounded-memory design.
func TestReconnect_DiscardedTerminalNotificationForgetsRevision(t *testing.T) {
	origBase, origMin, origMax := backoffBase, backoffMin, backoffMax
	backoffBase, backoffMin, backoffMax = 10*time.Millisecond, 5*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backoffBase, backoffMin, backoffMax = origBase, origMin, origMax })

	fs := newFakeServer(t, nil, func(conn *websocket.Conn, n int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
		if n == 1 {
			_ = conn.CloseNow()
			return
		}
		_, _ = serverReadEnvelope(conn) // block; test only cares about local state
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	// Give this session a known revision before it "completes", as a real
	// started/attached sequence would.
	c.revisions.Next("done-session")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	c.Start(ctx)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), testTimeout)
		defer closeCancel()
		_ = c.Close(closeCtx)
	}()

	// Enqueued while disconnected: will be discarded as stale on reconnect.
	c.Notify(SessionSnapshot{SessionID: "done-session", Status: StatusStopped, CreatedAt: time.Now()})

	waitFor(t, testTimeout, func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.connCount >= 2
	})
	time.Sleep(100 * time.Millisecond)

	if got := c.revisions.Current("done-session"); got != 0 {
		t.Fatalf("revisions.Current(done-session) = %d, want 0 (forgotten when its terminal notification was discarded)", got)
	}
}

// TestGracefulShutdown_FlushesPendingEvent guards against losing a final
// lifecycle notification (typically session_stopped) enqueued just before
// Close(): the Supervisor enqueues such notifications synchronously before
// calling Client.Close during shutdown, so they must have a chance to reach
// Bridge rather than sitting in a queue that's about to be torn down.
func TestGracefulShutdown_FlushesPendingEvent(t *testing.T) {
	flushed := make(chan envelope, 1)
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
		env, err := serverReadEnvelope(conn)
		if err != nil {
			return
		}
		_ = serverWriteEnvelope(conn, msgAck, "srv-ack", ackPayload{MessageID: env.MessageID, Result: "applied"})
		select {
		case flushed <- env:
		default:
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	c.Start(context.Background())

	// Wait for the connection to be up before racing Notify against Close.
	waitFor(t, testTimeout, func() bool {
		st, err := ReadStatus(c.cfg.StatusPath)
		return err == nil && st.State == StateConnected
	})

	c.Notify(SessionSnapshot{SessionID: "final-session", Status: StatusStopped, CreatedAt: time.Now()})
	closeCtx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := c.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case env := <-flushed:
		if env.Type != msgSessionStarted { // a never-before-seen session starts here
			t.Fatalf("expected session_started for the final notification, got %q", env.Type)
		}
		var sp sessionPayload
		if err := json.Unmarshal(env.Payload, &sp); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if sp.SessionID != "final-session" {
			t.Fatalf("session_id=%q, want final-session", sp.SessionID)
		}
	default:
		t.Fatal("the queued notification was never flushed before shutdown closed the connection")
	}
}

// --- silent/black-holed connection detection (no explicit close or error) ---

// TestConnectAndServe_SilentConnectionDetectedViaIdleTimeout guards against a
// network partition that never produces an explicit read/write error (e.g.
// a docker network disconnect or a NAT/firewall black hole): a TCP write can
// keep "succeeding" locally (accepted into the kernel send buffer) long
// after delivery has actually stopped, so the client must independently
// bound how long it will wait for a completely silent peer.
func TestConnectAndServe_SilentConnectionDetectedViaIdleTimeout(t *testing.T) {
	origMin := minIdleReadTimeout
	minIdleReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { minIdleReadTimeout = origMin })

	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 1); err != nil { // 1s heartbeat -> idle timeout still floors at minIdleReadTimeout
			return
		}
		// Go completely silent toward the client forever (never send another
		// message, never close) — but keep reading and discarding whatever
		// the client sends, so this handler returns (and httptest.Server's
		// deferred Close doesn't hang forever waiting for it) once the
		// client itself closes the connection after detecting the idle
		// timeout.
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	start := time.Now()
	err := c.connectAndServe(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a silent connection to eventually be detected as an error")
	}
	if elapsed > 4500*time.Millisecond {
		t.Fatalf("silent connection took %v to detect, want a bounded, short window", elapsed)
	}
}

// --- duplicate / stale event handling ---

// TestSendEvent_DuplicateAck_TreatedAsSuccess covers Bridge's "duplicate"
// ack result: an incremental event whose revision is <= the revision Bridge
// already has for that session (e.g. a retried send after an ambiguous
// network error) is acknowledged as "duplicate", not an error. The client
// must treat this exactly like "applied" and keep going.
func TestSendEvent_DuplicateAck_TreatedAsSuccess(t *testing.T) {
	var mu sync.Mutex
	var results []string
	snapshotAcked := make(chan struct{})
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
		close(snapshotAcked)
		first := true
		for {
			env, err := serverReadEnvelope(conn)
			if err != nil {
				return
			}
			mu.Lock()
			results = append(results, env.Type)
			mu.Unlock()
			result := "applied"
			if first {
				result = "duplicate" // Bridge treats this event as already known.
				first = false
			}
			_ = serverWriteEnvelope(conn, msgAck, "srv-ack", ackPayload{MessageID: env.MessageID, Result: result})
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	go func() { _ = c.connectAndServe(ctx) }()

	select {
	case <-snapshotAcked:
	case <-time.After(testTimeout):
		t.Fatal("server never received the initial snapshot")
	}
	time.Sleep(50 * time.Millisecond) // let the client's own post-snapshot drain finish first

	// Space the two Notify calls apart (waiting for the first to be recorded)
	// so they don't coalesce into a single pending entry for session "s1" —
	// coalescing repeated updates to the same session is the intended
	// behavior of Client.Notify, but this test is specifically about two
	// distinct wire messages surviving a duplicate ack on the first.
	c.Notify(SessionSnapshot{SessionID: "s1", Status: StatusRunning, CreatedAt: time.Now()})
	waitFor(t, testTimeout, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) >= 1
	})
	c.Notify(SessionSnapshot{SessionID: "s1", Status: StatusAttached, CreatedAt: time.Now()})

	waitFor(t, testTimeout, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) >= 2
	})
	// A "duplicate" ack on the first event must not have torn down the
	// connection or stopped subsequent sends: the second event still went
	// through as session_updated, not a retried session_started.
	mu.Lock()
	defer mu.Unlock()
	if results[0] != msgSessionStarted || results[1] != msgSessionUpdated {
		t.Fatalf("results=%v, want [session_started session_updated] despite a duplicate ack on the first", results)
	}
}

// TestSendSnapshot_RevisionNotAdvanced_StillAppliesSuccessfully covers
// Bridge's snapshot-level staleness semantics: a session_snapshot row whose
// revision does not exceed what Bridge already has for that session is
// silently no-op'd for that one row (the whole snapshot still acks
// "applied" — Bridge never fails a snapshot solely for a stale row). From
// the client's side this means sending the same snapshot twice in a row
// (e.g. two reconnects with no local state change) must never be treated as
// an error.
func TestSendSnapshot_RevisionNotAdvanced_StillAppliesSuccessfully(t *testing.T) {
	var snapshotAcks int32
	var snapshotRevisions []int64
	var mu sync.Mutex
	// Each connectAndServe call below dials a fresh connection, so the fake
	// server's handler runs once per connection: handshake, then exactly the
	// one session_snapshot a real client sends right after hello_ack.
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		_, snap, err := serverHandshake(conn, 30)
		if err != nil {
			return
		}
		var sp sessionSnapshotPayload
		if err := json.Unmarshal(snap.Payload, &sp); err == nil && len(sp.Sessions) == 1 {
			mu.Lock()
			snapshotRevisions = append(snapshotRevisions, sp.Sessions[0].Revision)
			mu.Unlock()
		}
		atomic.AddInt32(&snapshotAcks, 1)
	})
	defer fs.Close()

	snapshotFn := func() []SessionSnapshot {
		return []SessionSnapshot{{SessionID: "s1", Status: StatusRunning, CreatedAt: time.Now()}}
	}
	c := newTestClient(t, fs.wsURL(), "bri_valid", snapshotFn)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// First connectAndServe assigns and sends revision 1 (session never seen
	// before). A second, independent connection (as a real reconnect would
	// be) sends the *same* revision again, since sendSnapshot uses
	// RevisionStore.Current (peek), not Next (bump) — exactly Bridge's
	// "stale row in a snapshot" scenario. Neither call may error.
	// The fake server closes the connection right after acking the
	// snapshot (see serverHandshake), so connectAndServe returning an error
	// here is the harness disconnecting on purpose, not a failure worth
	// asserting on; only the server-observed snapshots matter below.
	_ = c.connectAndServe(ctx)
	_ = c.connectAndServe(ctx)

	if got := atomic.LoadInt32(&snapshotAcks); got != 2 {
		t.Fatalf("server observed %d snapshots, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(snapshotRevisions) != 2 || snapshotRevisions[0] != 1 || snapshotRevisions[1] != 1 {
		t.Fatalf("snapshot revisions=%v, want [1 1] (not re-advanced on the second, unchanged, connection)", snapshotRevisions)
	}
}

// --- credential revoked mid-connection ---

// TestConnectAndServe_CredentialRevokedMidConnection covers Bridge's
// documented "invalid_request"/"credential no longer valid" close: unlike
// an invalid credential rejected at the initial HTTP upgrade (401, tested
// separately), a credential revoked after a successful hello is only
// discovered lazily on the connection's next write, and closes with
// StatusPolicyViolation rather than a transport-level error.
func TestConnectAndServe_CredentialRevokedMidConnection(t *testing.T) {
	snapshotAcked := make(chan struct{})
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
		close(snapshotAcked)
		env, err := serverReadEnvelope(conn)
		if err != nil {
			return
		}
		_ = serverWriteEnvelope(conn, msgError, env.MessageID, errorPayload{
			MessageID: env.MessageID, Code: errCodeInvalidRequest, Message: "credential no longer valid",
		})
		_ = conn.Close(websocket.StatusPolicyViolation, "credential no longer valid")
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- c.connectAndServe(ctx) }()

	select {
	case <-snapshotAcked:
	case <-time.After(testTimeout):
		t.Fatal("server never received the initial snapshot")
	}
	time.Sleep(50 * time.Millisecond) // let the client's own post-snapshot drain finish first

	// Trigger the write that discovers the revocation.
	c.Notify(SessionSnapshot{SessionID: "s1", Status: StatusRunning, CreatedAt: time.Now()})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error once the credential is revoked mid-connection")
		}
		if got := classifyStatus(err); got != StateAuthRejected {
			t.Fatalf("classifyStatus(%v) = %v, want StateAuthRejected", err, got)
		}
	case <-time.After(testTimeout):
		t.Fatal("connectAndServe never returned after the mid-connection revocation")
	}
}

// --- bounded backoff ---

func TestBackoffDelay_Bounded(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for attempt := 1; attempt <= 30; attempt++ {
		d := backoffDelay(attempt, rng)
		if d < backoffMin || d > backoffMax {
			t.Fatalf("attempt %d: backoffDelay = %v, want within [%v, %v]", attempt, d, backoffMin, backoffMax)
		}
	}
}

// --- bounded memory / non-blocking Notify ---

func TestNotify_NeverBlocks(t *testing.T) {
	c := newTestClient(t, "ws://unused.invalid/v1/control", "bri_valid", nil)
	start := time.Now()
	for i := 0; i < 10_000; i++ {
		c.Notify(SessionSnapshot{SessionID: fmt.Sprintf("s%d", i), Status: StatusRunning})
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("10000 Notify calls with no consumer took %v, want fast/non-blocking", elapsed)
	}
	c.mu.Lock()
	pendingLen := len(c.pending)
	c.mu.Unlock()
	if pendingLen > eventQueueSize {
		t.Fatalf("pending map length %d exceeds bound %d", pendingLen, eventQueueSize)
	}
}

// TestNotify_CoalescesRepeatedUpdatesForSameSession guards the core premise
// of the pending map: many Notify calls for the *same* session_id before
// it's drained must never grow unboundedly or lose the latest state to a
// FIFO-style eviction — they coalesce into one pending entry holding only
// the most recent status.
func TestNotify_CoalescesRepeatedUpdatesForSameSession(t *testing.T) {
	c := newTestClient(t, "ws://unused.invalid/v1/control", "bri_valid", nil)
	for i := 0; i < 1000; i++ {
		c.Notify(SessionSnapshot{SessionID: "s1", Status: StatusRunning, CreatedAt: time.Now()})
	}
	c.Notify(SessionSnapshot{SessionID: "s1", Status: StatusStopped, CreatedAt: time.Now()})

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 1 {
		t.Fatalf("pending map has %d entries, want 1 (coalesced)", len(c.pending))
	}
	if got := c.pending["s1"].info.Status; got != StatusStopped {
		t.Fatalf("coalesced status = %q, want %q (the latest)", got, StatusStopped)
	}
}

// TestNotify_OverflowEvictsOldestAndSchedulesReconcile guards the fix for
// silently losing a session's only pending notification under overflow: once
// eventQueueSize distinct sessions are already pending, a new distinct
// session evicts the oldest one, but — unlike a plain drop — also schedules
// a fresh authoritative snapshot so the evicted session's true state is
// re-derived from the Supervisor rather than left stale indefinitely.
func TestNotify_OverflowEvictsOldestAndSchedulesReconcile(t *testing.T) {
	c := newTestClient(t, "ws://unused.invalid/v1/control", "bri_valid", nil)
	for i := 0; i < eventQueueSize; i++ {
		c.Notify(SessionSnapshot{SessionID: fmt.Sprintf("s%d", i), Status: StatusRunning, CreatedAt: time.Now()})
		time.Sleep(time.Millisecond) // ensure distinct, increasing enqueuedAt
	}
	c.Notify(SessionSnapshot{SessionID: "overflow", Status: StatusRunning, CreatedAt: time.Now()})

	c.mu.Lock()
	_, oldestStillPending := c.pending["s0"]
	pendingLen := len(c.pending)
	c.mu.Unlock()
	if oldestStillPending {
		t.Fatal("expected the oldest pending session (s0) to be evicted")
	}
	if pendingLen != eventQueueSize {
		t.Fatalf("pending map length = %d, want %d (bounded)", pendingLen, eventQueueSize)
	}
	select {
	case <-c.reconcileCh:
	default:
		t.Fatal("expected overflow to schedule a reconcile")
	}
}

// --- cancellation ---

func TestClient_CloseIsPromptAndIdempotent(t *testing.T) {
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
		_, _ = serverReadEnvelope(conn) // block until client closes
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	ctx := context.Background()
	c.Start(ctx)

	waitFor(t, testTimeout, func() bool {
		st, err := ReadStatus(c.cfg.StatusPath)
		return err == nil && st.State == StateConnected
	})

	closeCtx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := c.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent: a second Close must not panic or block.
	if err := c.Close(closeCtx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// --- telemetry/local operations remain independent: classifyStatus never panics ---

func TestClassifyStatus_HandlesNilAndUnknown(t *testing.T) {
	if got := classifyStatus(nil); got != StateDisconnected {
		t.Fatalf("classifyStatus(nil) = %v, want StateDisconnected", got)
	}
	if got := classifyStatus(fmt.Errorf("some unrelated error")); got != StateDisconnected {
		t.Fatalf("classifyStatus(unrelated) = %v, want StateDisconnected", got)
	}
}
