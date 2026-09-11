package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orchael/bridgectl/internal/localserver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFormatStartSessionError_SetupCommandFailed(t *testing.T) {
	grpcErr := status.Errorf(codes.FailedPrecondition,
		"start session: repo setup failed: command failed: exit status 254; output: line one\nline two\nline three")

	got := formatStartSessionError(grpcErr)
	msg := got.Error()

	if !strings.HasPrefix(msg, "repo setup failed") {
		t.Fatalf("expected prefix 'repo setup failed', got: %s", msg)
	}
	if !strings.Contains(msg, "exit status 254") {
		t.Fatalf("expected exit status, got: %s", msg)
	}
	if !strings.Contains(msg, "    line one") {
		t.Fatalf("expected indented output, got: %s", msg)
	}
	if strings.Contains(msg, "rpc error") {
		t.Fatalf("should not contain gRPC framing, got: %s", msg)
	}
	if strings.Count(msg, "start session") > 0 {
		t.Fatalf("should not contain 'start session', got: %s", msg)
	}
}

func TestFormatStartSessionError_SetupTimeout(t *testing.T) {
	grpcErr := status.Errorf(codes.FailedPrecondition,
		"start session: repo setup failed: setup timed out after 2m0s")

	got := formatStartSessionError(grpcErr)
	msg := got.Error()

	if !strings.Contains(msg, "setup timed out after 2m0s") {
		t.Fatalf("expected timeout message, got: %s", msg)
	}
}

func TestFormatStartSessionError_NonSetupError(t *testing.T) {
	grpcErr := status.Errorf(codes.Unavailable, "provider unavailable")
	got := formatStartSessionError(grpcErr)
	msg := got.Error()

	if !strings.Contains(msg, "start session") {
		t.Fatalf("non-setup errors should keep 'start session' prefix, got: %s", msg)
	}
	if !strings.Contains(msg, "provider unavailable") {
		t.Fatalf("should preserve original message, got: %s", msg)
	}
}

func TestFormatStartSessionError_NonGRPC(t *testing.T) {
	plainErr := errors.New("connection refused")
	got := formatStartSessionError(plainErr)
	msg := got.Error()

	if !strings.HasPrefix(msg, "start session:") {
		t.Fatalf("non-gRPC errors should keep prefix, got: %s", msg)
	}
}

func TestCodexAuthExpiredError(t *testing.T) {
	err := codexAuthExpiredError("codex", `{"code":"token_expired","message":"Provided authentication token is expired."}`)
	if err == nil {
		t.Fatal("expected expired auth error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "account auth is expired") {
		t.Fatalf("message should mention account auth expiry, got: %s", msg)
	}
	if !strings.Contains(msg, "auth.json") {
		t.Fatalf("message should mention auth.json refresh, got: %s", msg)
	}
	if !strings.Contains(msg, "reload credentials") || strings.Contains(msg, "remove CODEX_AUTH") {
		t.Fatalf("message should explain rotation of cached credentials, got: %s", msg)
	}
}

func TestCodexAuthExpiredErrorNonCodex(t *testing.T) {
	if err := codexAuthExpiredError("claude", "token_expired"); err != nil {
		t.Fatalf("non-codex provider should not get codex auth error: %v", err)
	}
}

func TestSecureServerDiscoveryError(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", stateDir)
	if err := os.WriteFile(filepath.Join(stateDir, "server.mode"), []byte(string(localserver.ModeSecure)+"\n"), 0o644); err != nil {
		t.Fatalf("write server.mode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "server.addr"), []byte("100.64.0.1:9445\n"), 0o644); err != nil {
		t.Fatalf("write server.addr: %v", err)
	}

	err := secureServerDiscoveryError()
	if err == nil {
		t.Fatal("expected secure discovery error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "100.64.0.1:9445") {
		t.Fatalf("message should include recorded addr, got: %s", msg)
	}
	if strings.Contains(msg, "5s") {
		t.Fatalf("message should not be generic 5s timeout, got: %s", msg)
	}
}
