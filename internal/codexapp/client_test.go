package codexapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orchael/bridgectl/internal/bridge"
)

const testTimeout = 5 * time.Second

// fakeAppServer is a minimal, scriptable stand-in for `codex app-server`,
// speaking the exact newline-delimited JSON-RPC 2.0 shapes captured from a
// real app-server process (see protocol.go's doc comment).
type fakeAppServer struct {
	srv    *httptest.Server
	connCh chan *websocket.Conn
}

func newFakeAppServer(t *testing.T) *fakeAppServer {
	t.Helper()
	fs := &fakeAppServer{connCh: make(chan *websocket.Conn, 4)}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		fs.connCh <- conn
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeAppServer) wsURL() string {
	return "ws" + strings.TrimPrefix(fs.srv.URL, "http")
}

func (fs *fakeAppServer) acceptConn(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-fs.connCh:
		return c
	case <-time.After(testTimeout):
		t.Fatal("no connection accepted")
		return nil
	}
}

func serverReadEnvelope(t *testing.T, conn *websocket.Conn) envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("server decode: %v", err)
	}
	return env
}

func serverSend(t *testing.T, conn *websocket.Conn, env envelope) {
	t.Helper()
	env.JSONRPC = "2.0"
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("server write: %v", err)
	}
}

func serverInitializeAck(t *testing.T, conn *websocket.Conn, reqID json.RawMessage) {
	t.Helper()
	serverSend(t, conn, envelope{ID: reqID, Result: mustJSON(map[string]any{"codexHome": "/tmp"})})
}

func notif(method string, params any) envelope {
	return envelope{Method: method, Params: mustJSON(params)}
}

func recvInteraction(t *testing.T, ch <-chan bridge.Interaction) bridge.Interaction {
	t.Helper()
	select {
	case i := <-ch:
		return i
	case <-time.After(testTimeout):
		t.Fatal("no interaction received")
		return bridge.Interaction{}
	}
}

func drainInitialize(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	env := serverReadEnvelope(t, conn)
	if env.Method != methodInitialize {
		t.Fatalf("first message = %q, want initialize", env.Method)
	}
	serverInitializeAck(t, conn, env.ID)
}

func TestWatch_InitializeHandshake(t *testing.T) {
	fs := newFakeAppServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chCh := make(chan (<-chan bridge.Interaction), 1)
	go func() {
		ch, err := Watch(ctx, fs.wsURL(), nil)
		if err != nil {
			t.Errorf("Watch: %v", err)
			return
		}
		chCh <- ch
	}()

	conn := fs.acceptConn(t)
	drainInitialize(t, conn)

	select {
	case <-chCh:
	case <-time.After(testTimeout):
		t.Fatal("Watch never returned a channel")
	}
}

func TestWatch_IdleFromThreadStarted(t *testing.T) {
	fs := newFakeAppServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, conn := startWatch(t, ctx, fs)

	serverSend(t, conn, notif(methodThreadStarted, threadStartedParams{
		Thread: threadRef{ID: "thread-1", Status: threadStatusPayload{Type: threadStatusIdle}},
	}))

	got := recvInteraction(t, ch)
	if got.EffectiveState() != bridge.InteractionIdle {
		t.Fatalf("state = %q, want idle", got.EffectiveState())
	}
	if got.Evidence.Source != evidenceSource {
		t.Fatalf("source = %q, want %q", got.Evidence.Source, evidenceSource)
	}
	if !got.Evidence.Capability.ApprovalStateSupported {
		t.Fatal("capability.ApprovalStateSupported = false, want true")
	}
}

func TestWatch_ApprovalRequestThenWaitingStatus_CarriesPendingRequest(t *testing.T) {
	fs := newFakeAppServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, conn := startWatch(t, ctx, fs)

	serverSend(t, conn, notif(methodThreadStarted, threadStartedParams{
		Thread: threadRef{ID: "thread-1", Status: threadStatusPayload{Type: threadStatusActive}},
	}))
	_ = recvInteraction(t, ch) // Working

	// The server sends an approval REQUEST (has an ID) — this client must
	// never respond to it, only observe it for identity/summary.
	serverSend(t, conn, envelope{
		ID:     json.RawMessage(`99`),
		Method: methodExecCommandApproval,
		Params: mustJSON(execCommandApprovalParams{CallID: "call-1", Command: []string{"rm", "-rf", "build/"}}),
	})
	serverSend(t, conn, notif(methodThreadStatusChanged, threadStatusChangedParams{
		ThreadID: "thread-1",
		Status:   threadStatusPayload{Type: threadStatusActive, ActiveFlags: []string{activeFlagWaitingOnApproval}},
	}))

	got := recvInteraction(t, ch)
	if got.EffectiveState() != bridge.InteractionWaitingForApproval {
		t.Fatalf("state = %q, want waiting_for_approval", got.EffectiveState())
	}
	if got.Pending == nil || got.Pending.ID != "call-1" || got.Pending.Type != bridge.PendingRequestApproval {
		t.Fatalf("Pending = %+v, want call-1/approval", got.Pending)
	}
	if !strings.Contains(got.Pending.Summary, "rm -rf build/") {
		t.Fatalf("Summary = %q, want it to mention the command", got.Pending.Summary)
	}

	// Authoritative resolution: status flips back to plain active (no
	// flags) — this alone must clear Pending, never a local WriteInput.
	serverSend(t, conn, notif(methodThreadStatusChanged, threadStatusChangedParams{
		ThreadID: "thread-1",
		Status:   threadStatusPayload{Type: threadStatusActive},
	}))
	resolved := recvInteraction(t, ch)
	if resolved.EffectiveState() != bridge.InteractionWorking {
		t.Fatalf("state after resolution = %q, want working", resolved.EffectiveState())
	}
	if resolved.Pending != nil {
		t.Fatalf("Pending after resolution = %+v, want nil", resolved.Pending)
	}
}

func TestWatch_ToolRequestUserInput_CarriesQuestionAsSummary(t *testing.T) {
	fs := newFakeAppServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, conn := startWatch(t, ctx, fs)

	serverSend(t, conn, notif(methodThreadStarted, threadStartedParams{
		Thread: threadRef{ID: "thread-1", Status: threadStatusPayload{Type: threadStatusActive}},
	}))
	_ = recvInteraction(t, ch)

	serverSend(t, conn, envelope{
		ID:     json.RawMessage(`100`),
		Method: methodItemToolRequestUserInput,
		Params: mustJSON(toolRequestUserInputParams{
			ItemID:    "item-1",
			Questions: []toolRequestUserInputQuestion{{Text: "Which environment should I target?"}},
		}),
	})
	serverSend(t, conn, notif(methodThreadStatusChanged, threadStatusChangedParams{
		ThreadID: "thread-1",
		Status:   threadStatusPayload{Type: threadStatusActive, ActiveFlags: []string{activeFlagWaitingOnUserInput}},
	}))

	got := recvInteraction(t, ch)
	if got.EffectiveState() != bridge.InteractionWaitingForInput {
		t.Fatalf("state = %q, want waiting_for_input", got.EffectiveState())
	}
	if got.Pending == nil || got.Pending.ID != "item-1" || got.Pending.Type != bridge.PendingRequestInput {
		t.Fatalf("Pending = %+v, want item-1/input", got.Pending)
	}
	if got.Pending.Summary != "Which environment should I target?" {
		t.Fatalf("Summary = %q, want the question text", got.Pending.Summary)
	}
}

func TestWatch_SecondThreadIgnored(t *testing.T) {
	fs := newFakeAppServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, conn := startWatch(t, ctx, fs)

	serverSend(t, conn, notif(methodThreadStarted, threadStartedParams{
		Thread: threadRef{ID: "thread-1", Status: threadStatusPayload{Type: threadStatusIdle}},
	}))
	first := recvInteraction(t, ch)
	if first.EffectiveState() != bridge.InteractionIdle {
		t.Fatal("expected idle from first thread")
	}

	// A second, unrelated thread must never affect this observer's state.
	serverSend(t, conn, notif(methodThreadStatusChanged, threadStatusChangedParams{
		ThreadID: "thread-2",
		Status:   threadStatusPayload{Type: threadStatusActive, ActiveFlags: []string{activeFlagWaitingOnApproval}},
	}))
	// Confirm no event arrives for thread-2 by sending a legitimate thread-1
	// event afterward and checking it's the very next thing received.
	serverSend(t, conn, notif(methodThreadStatusChanged, threadStatusChangedParams{
		ThreadID: "thread-1",
		Status:   threadStatusPayload{Type: threadStatusActive},
	}))
	next := recvInteraction(t, ch)
	if next.EffectiveState() != bridge.InteractionWorking {
		t.Fatalf("state = %q, want working (thread-2's event must have been ignored)", next.EffectiveState())
	}
}

func TestWatch_MalformedMessagesIgnored(t *testing.T) {
	fs := newFakeAppServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, conn := startWatch(t, ctx, fs)

	rawCtx, rawCancel := context.WithTimeout(context.Background(), testTimeout)
	_ = conn.Write(rawCtx, websocket.MessageText, []byte("not json"))
	rawCancel()

	serverSend(t, conn, notif(methodThreadStatusChanged, threadStatusChangedParams{
		ThreadID: "unknown-thread", Status: threadStatusPayload{Type: threadStatusIdle},
	})) // no thread adopted yet, but not thread-1 either — still fine, becomes adopted

	serverSend(t, conn, notif(methodThreadStarted, threadStartedParams{
		Thread: threadRef{ID: "thread-1", Status: threadStatusPayload{Type: threadStatusIdle}},
	}))

	// One of the two idle notifications above must surface; the important
	// property is that malformed JSON never crashes or blocks the observer.
	got := recvInteraction(t, ch)
	if got.EffectiveState() != bridge.InteractionIdle {
		t.Fatalf("state = %q, want idle", got.EffectiveState())
	}
}

func TestWatch_UnknownStatusType_ReportsUnknown(t *testing.T) {
	fs := newFakeAppServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, conn := startWatch(t, ctx, fs)

	serverSend(t, conn, notif(methodThreadStarted, threadStartedParams{
		Thread: threadRef{ID: "thread-1", Status: threadStatusPayload{Type: threadStatusSystemError}},
	}))
	got := recvInteraction(t, ch)
	if got.EffectiveState() != bridge.InteractionUnknown {
		t.Fatalf("state = %q, want unknown for systemError", got.EffectiveState())
	}
}

func TestWatch_ConnectionClosed_ChannelCloses(t *testing.T) {
	fs := newFakeAppServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, conn := startWatch(t, ctx, fs)
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to close, got a value instead")
		}
	case <-time.After(testTimeout):
		t.Fatal("channel never closed after server disconnect")
	}
}

func TestWatch_DialFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Watch(ctx, "ws://127.0.0.1:1/does-not-exist", nil)
	if err == nil {
		t.Fatal("expected an error dialing an unreachable endpoint")
	}
}

// watchResult carries Watch's return values across goroutines.
type watchResult struct {
	ch  <-chan bridge.Interaction
	err error
}

// startWatchAsync kicks off Watch in a goroutine without waiting for it:
// Watch blocks on the initialize round-trip, which only completes once the
// caller separately accepts the connection and drains the handshake (see
// finishWatch). Starting synchronously would deadlock against that.
func startWatchAsync(ctx context.Context, fs *fakeAppServer) <-chan watchResult {
	resCh := make(chan watchResult, 1)
	go func() {
		ch, err := Watch(ctx, fs.wsURL(), nil)
		resCh <- watchResult{ch, err}
	}()
	return resCh
}

// startWatch is the common case: start the client, accept its connection,
// complete the handshake, and return the resulting channel and the
// server-side connection (for the test to send further scripted messages).
func startWatch(t *testing.T, ctx context.Context, fs *fakeAppServer) (<-chan bridge.Interaction, *websocket.Conn) {
	t.Helper()
	resCh := startWatchAsync(ctx, fs)
	conn := fs.acceptConn(t)
	drainInitialize(t, conn)
	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("Watch: %v", r.err)
		}
		return r.ch, conn
	case <-time.After(testTimeout):
		t.Fatal("Watch did not return in time")
		return nil, nil
	}
}
