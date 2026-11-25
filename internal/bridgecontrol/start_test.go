package bridgecontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStartClaimIsDurableAndRejectsChangedPayload(t *testing.T) {
	org, inst := uuid.NewString(), uuid.NewString()
	count := 0
	c := New(Config{Credential: "test-credential", CommandPath: t.TempDir(), StartFunc: func(context.Context, StartRequest) error { count++; return nil }})
	c.organizationID, c.installationID = org, inst
	p := StartRequest{ID: uuid.NewString(), OrganizationID: org, InstallationID: inst, UserID: uuid.NewString(), SessionID: uuid.NewString(), Directory: "/tmp/project", ProjectID: "project", Provider: "codex", ExpiresAt: time.Now().Add(15 * time.Second)}
	if got := c.executeStart(context.Background(), p); got.Status != "accepted" {
		t.Fatalf("first start: %+v", got)
	}
	if got := c.executeStart(context.Background(), p); got.Status != "accepted" || count != 1 {
		t.Fatalf("replay: %+v, calls %d", got, count)
	}
	raw, err := os.ReadFile(filepath.Join(c.cfg.CommandPath, p.ID+".json"))
	if err != nil || strings.Contains(string(raw), p.Directory) {
		t.Fatalf("ledger exposed directory or could not be read: %v", err)
	}
	p.Directory = "/tmp/other"
	if got := c.executeStart(context.Background(), p); got.Code != "idempotency_conflict" || count != 1 {
		t.Fatalf("changed replay: %+v, calls %d", got, count)
	}
}

func TestStartUnknownIsNotRetriedAfterRestart(t *testing.T) {
	count := 0
	cfg := Config{Credential: "test", CommandPath: t.TempDir(), StartFunc: func(context.Context, StartRequest) error { count++; return errors.New("ambiguous") }}
	org, inst := uuid.NewString(), uuid.NewString()
	p := StartRequest{ID: uuid.NewString(), OrganizationID: org, InstallationID: inst, UserID: uuid.NewString(), SessionID: uuid.NewString(), Directory: "/tmp", ProjectID: "p", Provider: "codex", ExpiresAt: time.Now().Add(15 * time.Second)}
	for range 2 {
		c := New(cfg)
		c.organizationID, c.installationID = org, inst
		if got := c.executeStart(context.Background(), p); got.Status != "unknown" {
			t.Fatalf("unexpected: %+v", got)
		}
	}
	if count != 1 {
		t.Fatalf("start invoked %d times", count)
	}
}

func TestStartDistinguishesExplicitRejectionsFromAmbiguousOutcomes(t *testing.T) {
	for _, tc := range []struct {
		code           codes.Code
		status, result string
	}{
		{codes.InvalidArgument, "rejected", "invalid_request"},
		{codes.PermissionDenied, "rejected", "permission_denied"},
		{codes.ResourceExhausted, "rejected", "resource_limit"},
		{codes.FailedPrecondition, "rejected", "setup_failed"},
		{codes.Unavailable, "rejected", "server_or_provider_unavailable"},
		{codes.AlreadyExists, "unknown", "delivery_unknown"},
		{codes.DeadlineExceeded, "unknown", "delivery_unknown"},
		{codes.Internal, "unknown", "delivery_unknown"},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			org, inst := uuid.NewString(), uuid.NewString()
			c := New(Config{Credential: "test", CommandPath: t.TempDir(), StartFunc: func(context.Context, StartRequest) error { return status.Error(tc.code, "private detail") }})
			c.organizationID, c.installationID = org, inst
			p := StartRequest{ID: uuid.NewString(), OrganizationID: org, InstallationID: inst, UserID: uuid.NewString(), SessionID: uuid.NewString(), Directory: "/tmp", ProjectID: "project", Provider: "codex", ExpiresAt: time.Now().Add(15 * time.Second)}
			got := c.executeStart(context.Background(), p)
			if got.Status != tc.status || got.Code != tc.result {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestStartRejectsWrongInstallation(t *testing.T) {
	c := New(Config{Credential: "test", CommandPath: t.TempDir(), StartFunc: func(context.Context, StartRequest) error { t.Fatal("dispatched to wrong installation"); return nil }})
	c.organizationID, c.installationID = uuid.NewString(), uuid.NewString()
	p := StartRequest{ID: uuid.NewString(), OrganizationID: c.organizationID, InstallationID: uuid.NewString(), UserID: uuid.NewString(), SessionID: uuid.NewString(), Directory: "/tmp", ProjectID: "p", Provider: "codex", ExpiresAt: time.Now().Add(time.Second)}
	if got := c.executeStart(context.Background(), p); got.Code != "wrong_installation" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestStartWorkerDrainsOnCloseWithoutBlockingCommands(t *testing.T) {
	org, inst := uuid.NewString(), uuid.NewString()
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := New(Config{Endpoint: "ws://127.0.0.1:1", Credential: "test", CommandPath: t.TempDir(),
		StartFunc: func(ctx context.Context, _ StartRequest) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return ctx.Err()
		},
		RespondFunc: func(context.Context, string, string, string) error { return nil },
	})
	c.organizationID, c.installationID = org, inst
	p := StartRequest{ID: uuid.NewString(), OrganizationID: org, InstallationID: inst, UserID: uuid.NewString(), SessionID: uuid.NewString(), Directory: "/tmp", ProjectID: "p", Provider: "codex", ExpiresAt: time.Now().Add(15 * time.Second)}
	c.Start(context.Background())
	if !c.launchStartWorker(func(ctx context.Context) { c.executeStart(ctx, p) }) {
		t.Fatal("worker not launched")
	}
	<-started
	cmd := Command{ID: uuid.NewString(), OrganizationID: org, InstallationID: inst, SessionID: "s", UserID: "u", Action: "respond", PendingRequestID: "p", Text: "answer", ExpiresAt: time.Now().Add(time.Second)}
	commandDone := make(chan CommandResult, 1)
	go func() { commandDone <- c.executeCommand(context.Background(), cmd) }()
	select {
	case result := <-commandDone:
		if result.Status != "accepted" {
			t.Fatalf("command: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("start blocked unrelated command")
	}
	done := make(chan error, 1)
	go func() { done <- c.Close(context.Background()) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("worker not canceled")
	}
	select {
	case <-done:
		t.Fatal("close returned before worker drained")
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close did not drain")
	}
	if c.launchStartWorker(func(context.Context) { t.Error("launched after close") }) {
		t.Fatal("worker accepted after close")
	}
}

func TestConcurrentStartAndReplayDoNotWaitForSlowStart(t *testing.T) {
	org, inst := uuid.NewString(), uuid.NewString()
	entered, release := make(chan struct{}), make(chan struct{})
	c := New(Config{Credential: "test", CommandPath: t.TempDir(), StartFunc: func(_ context.Context, p StartRequest) error {
		if p.ProjectID == "slow" {
			close(entered)
			<-release
		}
		return nil
	}})
	c.organizationID, c.installationID = org, inst
	p := StartRequest{ID: uuid.NewString(), OrganizationID: org, InstallationID: inst, UserID: uuid.NewString(), SessionID: uuid.NewString(), Directory: "/tmp", ProjectID: "slow", Provider: "codex", ExpiresAt: time.Now().Add(15 * time.Second)}
	done := make(chan CommandResult, 1)
	go func() { done <- c.executeStart(context.Background(), p) }()
	<-entered
	other := p
	other.ID, other.SessionID, other.ProjectID = uuid.NewString(), uuid.NewString(), "other"
	otherDone, replayDone := make(chan CommandResult, 1), make(chan CommandResult, 1)
	go func() { otherDone <- c.executeStart(context.Background(), other) }()
	go func() { replayDone <- c.executeStart(context.Background(), p) }()
	select {
	case result := <-otherDone:
		if result.Status != "accepted" {
			t.Fatalf("other: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated start blocked")
	}
	select {
	case result := <-replayDone:
		if result.Status != "unknown" {
			t.Fatalf("replay: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("replay blocked")
	}
	close(release)
	if result := <-done; result.Status != "accepted" {
		t.Fatalf("slow: %+v", result)
	}
}
