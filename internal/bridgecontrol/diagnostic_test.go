package bridgecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orchael/bridgectl/internal/bridge"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func diagClient(f func(string) ([]byte, error)) *Client {
	c := New(Config{DiagnoseFunc: f})
	c.organizationID, c.installationID = "o", "i"
	return c
}

func TestDiagnoseScopeAndCodes(t *testing.T) {
	calls := 0
	good := []byte(`{"schema_version":1}`)
	c := diagClient(func(id string) ([]byte, error) {
		calls++
		if id != "s" {
			t.Fatalf("session = %q", id)
		}
		return good, nil
	})
	req := DiagnosticRequest{ID: "r", OrganizationID: "o", InstallationID: "i", SessionID: "s"}

	for name, bad := range map[string]DiagnosticRequest{
		"other org":          {ID: "r", OrganizationID: "x", InstallationID: "i", SessionID: "s"},
		"other installation": {ID: "r", OrganizationID: "o", InstallationID: "x", SessionID: "s"},
		"no id":              {OrganizationID: "o", InstallationID: "i", SessionID: "s"},
		"long id":            {ID: strings.Repeat("a", 129), OrganizationID: "o", InstallationID: "i", SessionID: "s"},
		"no session":         {ID: "r", OrganizationID: "o", InstallationID: "i"},
		"long session":       {ID: "r", OrganizationID: "o", InstallationID: "i", SessionID: strings.Repeat("s", 257)},
	} {
		if got := c.diagnose(bad); got.Code != "invalid_request" || got.Report != nil || calls != 0 {
			t.Fatalf("%s: %+v (calls %d)", name, got, calls)
		}
	}

	got := c.diagnose(req)
	if got.Code != "" || got.ID != "r" || string(got.Report) != string(good) || calls != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestDiagnoseFailureCodesNeverCarryErrorText(t *testing.T) {
	req := DiagnosticRequest{ID: "r", OrganizationID: "o", InstallationID: "i", SessionID: "s"}
	cases := map[string]struct {
		f    func(string) ([]byte, error)
		want string
	}{
		"not found":   {func(string) ([]byte, error) { return nil, bridge.ErrSessionNotFound }, "not_found"},
		"other error": {func(string) ([]byte, error) { return nil, errors.New("SECRET provider stderr") }, "unavailable"},
		"empty":       {func(string) ([]byte, error) { return nil, nil }, "unavailable"},
		"not json":    {func(string) ([]byte, error) { return []byte("{nope"), nil }, "unavailable"},
		"oversized":   {func(string) ([]byte, error) { return []byte(`"` + strings.Repeat("x", maxDiagnosticBytes) + `"`), nil }, "unavailable"},
	}
	for name, tc := range cases {
		got := diagClient(tc.f).diagnose(req)
		raw, _ := json.Marshal(got)
		if got.Code != tc.want || got.Report != nil || strings.Contains(string(raw), "SECRET") {
			t.Errorf("%s: %s", name, raw)
		}
	}
	// Not configured: unsupported, and the capability is not advertised.
	c := New(Config{})
	c.organizationID, c.installationID = "o", "i"
	if got := c.diagnose(req); got.Code != "unsupported" {
		t.Fatalf("unconfigured: %+v", got)
	}
}

// TestDiagnoseOverWebSocket drives the real message loop: hello advertises the
// capability, a diagnose_session request is answered with session_diagnostic.
func TestDiagnoseOverWebSocket(t *testing.T) {
	capsCh := make(chan map[string]bool, 1)
	replyCh := make(chan DiagnosticResult, 1)
	fs := newFakeServer(t, nil, func(conn *websocket.Conn, _ int) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var hello envelope
		_ = json.Unmarshal(data, &hello)
		var p helloPayload
		_ = json.Unmarshal(hello.Payload, &p)
		caps := map[string]bool{}
		_ = json.Unmarshal(p.Capabilities, &caps)
		capsCh <- caps
		ack, _ := json.Marshal(envelope{ProtocolVersion: ProtocolVersion, Type: msgHelloAck, Payload: mustJSON(helloAckPayload{InstallationID: "i", OrganizationID: "o", ProtocolVersion: ProtocolVersion, HeartbeatIntervalSeconds: 30})})
		if conn.Write(ctx, websocket.MessageText, ack) != nil {
			return
		}
		req, _ := json.Marshal(envelope{ProtocolVersion: ProtocolVersion, Type: "diagnose_session", MessageID: "m1", Payload: mustJSON(DiagnosticRequest{ID: "req-1", OrganizationID: "o", InstallationID: "i", SessionID: "sess"})})
		if conn.Write(ctx, websocket.MessageText, req) != nil {
			return
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env envelope
			if json.Unmarshal(data, &env) == nil && env.Type == "session_diagnostic" {
				var res DiagnosticResult
				_ = json.Unmarshal(env.Payload, &res)
				replyCh <- res
				return
			}
		}
	})
	defer fs.Close()

	c := newTestClient(t, fs.wsURL(), "bri_valid", func() []SessionSnapshot { return nil })
	c.cfg.DiagnoseFunc = func(id string) ([]byte, error) {
		if id != "sess" {
			t.Errorf("session = %q", id)
		}
		return []byte(`{"schema_version":1,"session_id":"sess"}`), nil
	}
	c.Start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_ = c.Close(ctx)
	}()

	select {
	case caps := <-capsCh:
		if !caps["diagnose_session"] {
			t.Fatalf("hello did not advertise diagnose_session: %v", caps)
		}
	case <-time.After(testTimeout):
		t.Fatal("no hello")
	}
	select {
	case res := <-replyCh:
		if res.ID != "req-1" || res.Code != "" || !strings.Contains(string(res.Report), `"schema_version":1`) {
			t.Fatalf("reply = %+v", res)
		}
	case <-time.After(testTimeout):
		t.Fatal("no session_diagnostic reply")
	}
}
