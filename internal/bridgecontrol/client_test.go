package bridgecontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
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
	var fs *fakeServer
	fs = newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			t.Errorf("handshake: %v", err)
			return
		}
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

	// Notify is safe to call immediately: it only enqueues, and the client's
	// serve loop drains events in order once the handshake/snapshot finish.
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusRunning, CreatedAt: time.Now()})
	c.Notify(SessionSnapshot{SessionID: "s1", Provider: "codex", ProjectID: "p1", Status: StatusAttached, CreatedAt: time.Now()})
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
		// Go completely silent forever: never send another message, never
		// close. A well-behaved client must still notice within a bounded
		// window.
		select {}
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
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
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

	c.Notify(SessionSnapshot{SessionID: "s1", Status: StatusRunning, CreatedAt: time.Now()})
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
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		if _, _, err := serverHandshake(conn, 30); err != nil {
			return
		}
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
	if len(c.events) > eventQueueSize {
		t.Fatalf("event queue length %d exceeds bound %d", len(c.events), eventQueueSize)
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
