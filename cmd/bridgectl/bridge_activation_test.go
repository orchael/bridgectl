package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/spf13/cobra"

	"github.com/orchael/bridgectl/internal/bridgecontrol"
)

func TestWaitForBridgeControlRequiresFreshMatchingConnection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state bridgecontrol.State
		id    string
		age   time.Duration
		ok    bool
	}{
		{"connected", bridgecontrol.StateConnected, "installation", 0, true},
		{"other installation", bridgecontrol.StateConnected, "other", 0, false},
		{"stale", bridgecontrol.StateConnected, "installation", 3 * time.Minute, false},
		{"rejected", bridgecontrol.StateAuthRejected, "installation", 0, false},
		{"disconnected", bridgecontrol.StateDisconnected, "installation", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := bridgecontrol.WriteStatus(filepath.Join(dir, "bridge-control-status.json"), bridgecontrol.Status{State: tc.state, InstallationID: tc.id, UpdatedAt: time.Now().Add(-tc.age)})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err = waitForBridgeControl(ctx, dir, "installation")
			if (err == nil) != tc.ok {
				t.Fatalf("wait error = %v, want success %v", err, tc.ok)
			}
		})
	}
}

// Exercise the real login activation path, a running daemon,
// and an authenticated WSS handshake plus its initial session snapshot.
func TestActivateBridgeEnrollmentConnectsRunningDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshots := make(chan []string, 4)
	tok := newControlDeviceToken()
	tok.InstallationID = "installation"
	controlServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok.ControlCredential {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		var hello map[string]any
		if err := wsjson.Read(ctx, conn, &hello); err != nil {
			return
		}
		if hello["type"] != "hello" {
			t.Error("expected control hello")
			return
		}
		if err := wsjson.Write(ctx, conn, map[string]any{"protocol_version": 1, "type": "hello_ack", "message_id": "hello-ack", "payload": map[string]any{"protocol_version": 1, "installation_id": "installation", "organization_id": "org", "heartbeat_interval_seconds": 30}}); err != nil {
			return
		}
		for {
			var event struct {
				Type      string `json:"type"`
				MessageID string `json:"message_id"`
				Payload   struct {
					Sessions []struct {
						ID string `json:"session_id"`
					} `json:"sessions"`
				} `json:"payload"`
			}
			if err := wsjson.Read(ctx, conn, &event); err != nil {
				return
			}
			if event.Type == "session_snapshot" {
				ids := []string{}
				for _, session := range event.Payload.Sessions {
					ids = append(ids, session.ID)
				}
				select {
				case snapshots <- ids:
				case <-ctx.Done():
					return
				}
			}
			if err := wsjson.Write(ctx, conn, map[string]any{"protocol_version": 1, "type": "ack", "message_id": "ack", "payload": map[string]any{"message_id": event.MessageID, "result": "applied"}}); err != nil {
				return
			}
		}
	}))
	defer controlServer.Close()
	// Scope test trust to this TLS fixture. Production still uses system roots.
	previousClient := http.DefaultClient
	http.DefaultClient = controlServer.Client()
	defer func() { http.DefaultClient = previousClient }()
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte("telemetry:\n  enabled: false\nproviders:\n  testprovider:\n    binary: cat\n    startup_probe: none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := localserver.Start(localserver.Config{StateDir: dir, ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	client, err := connectClient(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	preLoginSessionID := uuid.NewString()
	if _, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{SessionId: preLoginSessionID, ProjectId: "test", RepoPath: t.TempDir(), Provider: "testprovider"}); err != nil {
		t.Fatal(err)
	}
	tok.BridgeURL = controlServer.URL
	tok.TelemetryEndpoint = controlServer.URL + "/v1/telemetry/segments"
	tok.ControlEndpoint = "wss" + strings.TrimPrefix(controlServer.URL, "https") + "/v1/control"
	tok.OrganizationURL = ""
	if err := persistBridgeEnrollment(tok, "test", controlServer.URL); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	for range 2 {
		if err := activateBridgeEnrollment(ctx, cmd); err != nil {
			t.Fatal(err)
		}
		select {
		case ids := <-snapshots:
			if len(ids) != 1 || ids[0] != preLoginSessionID {
				t.Fatalf("live session missing from Bridge snapshot: %v", ids)
			}
		case <-ctx.Done():
			t.Fatal("no initial session snapshot")
		}
	}
	if !strings.Contains(out.String(), "Bridge control connected") {
		t.Fatalf("missing connection confirmation: %s", out.String())
	}
}
