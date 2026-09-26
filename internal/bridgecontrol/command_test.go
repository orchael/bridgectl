package bridgecontrol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orchael/bridgectl/internal/bridge"
)

func TestMAR66CommandReplayAndScope(t *testing.T) {
	calls := 0
	cfg := Config{Credential: "test-key", CommandPath: t.TempDir(), RespondFunc: func(context.Context, string, string, string) error { calls++; return nil }}
	client := New(cfg)
	client.organizationID = "org"
	client.installationID = "inst"
	cmd := Command{ID: uuid.NewString(), OrganizationID: "org", InstallationID: "inst", SessionID: "s", UserID: "user", Action: "respond", PendingRequestID: "p", ExpiresAt: time.Now().Add(20 * time.Second), Text: "private response"}
	for i := 0; i < 2; i++ {
		if result := client.executeCommand(context.Background(), cmd); result.Status != "accepted" {
			t.Fatalf("%+v", result)
		}
	}
	restarted := New(cfg)
	restarted.organizationID = "org"
	restarted.installationID = "inst"
	if result := restarted.executeCommand(context.Background(), cmd); result.Status != "accepted" {
		t.Fatalf("restart: %+v", result)
	}
	if calls != 1 {
		t.Fatalf("submitted %d times", calls)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.CommandPath, cmd.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cmd.Text) {
		t.Fatal("ledger retained text")
	}
	for _, tc := range []struct {
		name   string
		change func(*Command)
		code   string
	}{
		{"changed text", func(c *Command) { c.Text = "changed" }, "idempotency_conflict"},
		{"other session", func(c *Command) { c.SessionID = "other" }, "idempotency_conflict"},
		{"other request", func(c *Command) { c.PendingRequestID = "other" }, "idempotency_conflict"},
		{"other org", func(c *Command) { c.OrganizationID = "other" }, "wrong_installation"},
		{"other installation", func(c *Command) { c.InstallationID = "other" }, "wrong_installation"},
		{"oversize", func(c *Command) { c.Text = strings.Repeat("a", 16385) }, "invalid_command"},
		{"terminal escape", func(c *Command) { c.Text = "\x1by" }, "invalid_command"},
		{"approval", func(c *Command) { c.Action = "approve" }, "invalid_command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := cmd
			tc.change(&copy)
			if result := client.executeCommand(context.Background(), copy); result.Code != tc.code {
				t.Fatalf("%+v", result)
			}
		})
	}
	if calls != 1 {
		t.Fatal("rejection reached provider")
	}
}

func TestMAR66UnknownOutcomeNeverRetries(t *testing.T) {
	calls := 0
	c := New(Config{Credential: "test", CommandPath: t.TempDir(), RespondFunc: func(context.Context, string, string, string) error { calls++; return context.DeadlineExceeded }})
	c.organizationID = "o"
	c.installationID = "i"
	cmd := Command{ID: uuid.NewString(), OrganizationID: "o", InstallationID: "i", SessionID: "s", UserID: "u", Action: "respond", PendingRequestID: "p", ExpiresAt: time.Now().Add(time.Second), Text: "answer"}
	for i := 0; i < 2; i++ {
		if got := c.executeCommand(context.Background(), cmd); got.Status != "unknown" {
			t.Fatalf("%+v", got)
		}
	}
	if calls != 1 {
		t.Fatal("ambiguous delivery retried")
	}
}

func TestMAR66BoundedErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{{bridge.ErrWriterConflict, "writer_conflict"}, {bridge.ErrPendingRequestMismatch, "pending_request_mismatch"}, {bridge.ErrSessionNotRunning, "session_unavailable"}, {bridge.ErrRemoteResponseUnsupported, "unsupported"}} {
		c := New(Config{Credential: "test", CommandPath: t.TempDir(), RespondFunc: func(context.Context, string, string, string) error { return tc.err }})
		c.organizationID = "o"
		c.installationID = "i"
		cmd := Command{ID: uuid.NewString(), OrganizationID: "o", InstallationID: "i", SessionID: "s", UserID: "u", Action: "respond", PendingRequestID: "p", ExpiresAt: time.Now().Add(time.Second), Text: "answer"}
		if got := c.executeCommand(context.Background(), cmd); got.Code != tc.code {
			t.Fatalf("%+v", got)
		}
	}
}

func TestMAR66ApprovalReplay(t *testing.T) {
	calls := 0
	cfg := Config{Credential: "key", CommandPath: t.TempDir(), ApprovalFunc: func(context.Context, string, string, string) error { calls++; return nil }}
	c := New(cfg)
	c.organizationID = "o"
	c.installationID = "i"
	cmd := Command{ID: uuid.NewString(), OrganizationID: "o", InstallationID: "i", SessionID: "s", UserID: "u", Action: "approve", PendingRequestID: "rpc", Decision: "cancel", ExpiresAt: time.Now().Add(20 * time.Second)}
	for i := 0; i < 2; i++ {
		if got := c.executeCommand(context.Background(), cmd); got.Status != "accepted" {
			t.Fatal(got)
		}
	}
	restarted := New(cfg)
	restarted.organizationID = "o"
	restarted.installationID = "i"
	if got := restarted.executeCommand(context.Background(), cmd); got.Status != "accepted" {
		t.Fatal(got)
	}
	cmd.Decision = "accept"
	if got := c.executeCommand(context.Background(), cmd); got.Code != "idempotency_conflict" {
		t.Fatal(got)
	}
	cmd.Decision = "acceptForSession"
	if got := c.executeCommand(context.Background(), cmd); got.Code != "invalid_command" {
		t.Fatal(got)
	}
	if calls != 1 {
		t.Fatal("duplicate approval dispatched")
	}
}
