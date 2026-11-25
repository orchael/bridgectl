package localserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
	"github.com/orchael/bridgectl/internal/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRemoteStartUsesSessionRPCOnIsolatedServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "project")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(config, []byte("providers:\n  testprovider:\n    binary: cat\n    startup_probe: none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Config{StateDir: t.TempDir(), ConfigPath: config, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	start := newRemoteStartFunc(srv.supervisor, testLogger(), "test", server.RateLimitConfig{}, nil, nil)
	id := uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := start(ctx, bridgecontrol.StartRequest{SessionID: id, Directory: repo, Provider: "testprovider", ProjectID: "project"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	info, err := srv.supervisor.Get(id)
	if err != nil || info.SessionID != id {
		t.Fatalf("session missing: %+v %v", info, err)
	}
	if err := srv.supervisor.Stop(id, true); err != nil {
		t.Fatal(err)
	}

	limited := newRemoteStartFunc(srv.supervisor, testLogger(), "test", server.RateLimitConfig{StartSessionPerClientRPS: 0.0001, StartSessionPerClientBurst: 1}, nil, nil)
	firstUser, secondUser := uuid.NewString(), uuid.NewString()
	request := bridgecontrol.StartRequest{UserID: firstUser, SessionID: uuid.NewString(), Directory: filepath.Join(home, "missing"), Provider: "testprovider", ProjectID: "project-a"}
	if code := status.Code(limited(ctx, request)); code != codes.PermissionDenied {
		t.Fatalf("first user: %s", code)
	}
	request.ProjectID = "project-b"
	if code := status.Code(limited(ctx, request)); code != codes.ResourceExhausted {
		t.Fatalf("same user across projects: %s", code)
	}
	request.UserID, request.ProjectID = secondUser, "project-a"
	if code := status.Code(limited(ctx, request)); code != codes.PermissionDenied {
		t.Fatalf("second user in same project: %s", code)
	}

	global := server.RateLimitConfig{GlobalRPS: 0.0001, GlobalBurst: 1}
	shared := server.NewRateLimiters(global)
	localRPC := server.New(srv.supervisor, nil, testLogger(), global, "", nil, nil, "", "test")
	localRPC.SetRateLimiters(shared)
	_, _ = localRPC.StartSession(context.Background(), &bridgev1.StartSessionRequest{})
	remote := newRemoteStartFunc(srv.supervisor, testLogger(), "test", global, nil, shared)
	if code := status.Code(remote(ctx, request)); code != codes.ResourceExhausted {
		t.Fatalf("shared global budget: %s", code)
	}
}
