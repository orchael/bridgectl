package bridgecontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeServer is a minimal, scriptable stand-in for Bridge's control-plane
// WebSocket endpoint, used to test the bridgectl-side client against the
// exact wire protocol from merged Bridge PR #16 without a real Bridge
// deployment or database.
type fakeServer struct {
	t    *testing.T
	srv  *httptest.Server
	auth func(header string) bool // nil accepts any Authorization header

	mu        sync.Mutex
	connCount int
	msgs      []envelope
}

func newFakeServer(t *testing.T, auth func(string) bool, handler func(conn *websocket.Conn, connNum int)) *fakeServer {
	t.Helper()
	fs := &fakeServer{t: t, auth: auth}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fs.auth != nil && !fs.auth(r.Header.Get("Authorization")) {
			http.Error(w, "invalid control credential", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		fs.mu.Lock()
		fs.connCount++
		n := fs.connCount
		fs.mu.Unlock()
		if handler != nil {
			handler(conn, n)
		}
	}))
	return fs
}

func (fs *fakeServer) wsURL() string {
	return "ws" + strings.TrimPrefix(fs.srv.URL, "http")
}

func (fs *fakeServer) Close() { fs.srv.Close() }

func (fs *fakeServer) record(env envelope) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.msgs = append(fs.msgs, env)
}

func (fs *fakeServer) recorded() []envelope {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]envelope, len(fs.msgs))
	copy(out, fs.msgs)
	return out
}

const testTimeout = 5 * time.Second

func serverReadEnvelope(conn *websocket.Conn) (envelope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return envelope{}, err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return envelope{}, err
	}
	return env, nil
}

func serverWriteEnvelope(conn *websocket.Conn, msgType, messageID string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = b
	}
	env := envelope{ProtocolVersion: ProtocolVersion, Type: msgType, MessageID: messageID, Payload: raw}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

// serverHandshake reads a hello and replies with hello_ack, then reads and
// acks the mandatory post-hello session_snapshot. It returns the received
// hello and snapshot envelopes for the caller to assert on.
func serverHandshake(conn *websocket.Conn, heartbeatSeconds int) (hello, snapshot envelope, err error) {
	hello, err = serverReadEnvelope(conn)
	if err != nil {
		return
	}
	ack := helloAckPayload{
		InstallationID:           "install-1",
		OrganizationID:           "org-1",
		DisplayName:              "test-installation",
		ProtocolVersion:          ProtocolVersion,
		HeartbeatIntervalSeconds: heartbeatSeconds,
	}
	if err = serverWriteEnvelope(conn, msgHelloAck, "srv-hello-ack", ack); err != nil {
		return
	}
	snapshot, err = serverReadEnvelope(conn)
	if err != nil {
		return
	}
	err = serverWriteEnvelope(conn, msgAck, "srv-ack-snapshot", ackPayload{MessageID: snapshot.MessageID, Result: "applied"})
	return
}

func testCredential(expected string) func(string) bool {
	return func(header string) bool {
		return header == "Bearer "+expected
	}
}
