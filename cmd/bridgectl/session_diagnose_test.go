package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
	"github.com/orchael/bridgectl/internal/diagnose"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

const diagSessionID = "11111111-1111-4111-8111-111111111111"

// diagGetter adapts a canned GetSession result into a reportFetcher that goes
// through the same fallback path production uses against an older daemon.
func diagGetter(resp *bridgev1.GetSessionResponse, err error) reportFetcher {
	return func(ctx context.Context, id string) (*diagnose.Report, error) {
		return fetchReport(ctx, id,
			func(context.Context, string) ([]byte, error) {
				return nil, status.Error(codes.Unimplemented, "old daemon")
			},
			func(context.Context) string { return "daemon-old" },
			func(context.Context, string) (*bridgev1.GetSessionResponse, error) { return resp, err })
	}
}

func diagResp() *bridgev1.GetSessionResponse {
	return &bridgev1.GetSessionResponse{
		SessionId: diagSessionID, ProjectId: "proj", Provider: "claude",
		Status:    bridgev1.SessionStatus_SESSION_STATUS_RUNNING,
		CreatedAt: timestamppb.New(time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)),
		Error:     "SECRET-stderr", RepoPath: "/home/dev/SECRET-repo",
	}
}

func TestSessionDiagnoseJSON(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	var out bytes.Buffer
	if err := runSessionDiagnose(context.Background(), &out, diagSessionID, true, diagGetter(diagResp(), nil)); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("not a single JSON document: %v\n%s", err, out.String())
	}
	if m["schema_version"] != float64(1) || m["session_id"] != diagSessionID || m["status"] != "running" {
		t.Fatalf("unexpected report: %v", m)
	}
	if m["interaction_state"] != "unknown" {
		t.Fatalf("interaction_state = %v", m["interaction_state"])
	}
	if ctl := m["control"].(map[string]any); ctl["state"] != "unknown" {
		t.Fatalf("control without Bridge files must be unknown, got %v", ctl)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("leaked server-side free-form fields: %s", out.String())
	}
}

func TestSessionDiagnoseHumanMatchesJSONValues(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	var out bytes.Buffer
	if err := runSessionDiagnose(context.Background(), &out, diagSessionID, false, diagGetter(diagResp(), nil)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{diagSessionID, "claude", "proj", "running", "unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human output missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("leaked: %s", out.String())
	}
}

func TestSessionDiagnoseReadsControlFilesWhenBridgeDisconnected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRIDGECTL_STATE_DIR", dir)
	if err := bridgecontrol.WriteStatus(filepath.Join(dir, bridgecontrol.StatusFileName),
		bridgecontrol.Status{State: bridgecontrol.StateUnavailable, LastError: "SECRET-dial", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bridgecontrol.RevisionFileName), []byte(`{"`+diagSessionID+`":6}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runSessionDiagnose(context.Background(), &out, diagSessionID, true, diagGetter(diagResp(), nil)); err != nil {
		t.Fatalf("diagnose must work while Bridge is unavailable: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(out.Bytes(), &m)
	if m["control"].(map[string]any)["state"] != "unavailable" || m["lifecycle_revision_wire"] != float64(6) {
		t.Fatalf("report = %v", m)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("leaked last_error: %s", out.String())
	}
}

func TestSessionDiagnoseErrors(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		get      reportFetcher
		wantCode string
	}{
		{"malformed id", "not-a-uuid", diagGetter(nil, errors.New("must not be called")), "invalid_session_id"},
		{"empty id", "", diagGetter(nil, errors.New("must not be called")), "invalid_session_id"},
		{"nonexistent", "22222222-2222-4222-8222-222222222222", diagGetter(nil, bridgeclient.ErrSessionNotFound), "session_not_found"},
		{"server down", diagSessionID, diagGetter(nil, errServerUnavailable), "server_unavailable"},
		{"grpc unavailable", diagSessionID, diagGetter(nil, status.Error(codes.Unavailable, "dial SECRET")), "server_unavailable"},
		{"other", diagSessionID, diagGetter(nil, errors.New("boom SECRET")), "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, jsonOut := range []bool{true, false} {
				var out bytes.Buffer
				err := runSessionDiagnose(context.Background(), &out, tc.id, jsonOut, tc.get)
				if err == nil {
					t.Fatal("want non-zero exit (error)")
				}
				if strings.Contains(err.Error(), "SECRET") || strings.Contains(out.String(), "SECRET") {
					t.Fatalf("server error text leaked: %v %s", err, out.String())
				}
				if !jsonOut {
					if out.Len() != 0 {
						t.Fatalf("human mode must not write an empty snapshot: %q", out.String())
					}
					continue
				}
				var m struct {
					SchemaVersion int `json:"schema_version"`
					Error         struct{ Code, Message string }
				}
				if jerr := json.Unmarshal(out.Bytes(), &m); jerr != nil {
					t.Fatalf("error output not JSON: %v\n%s", jerr, out.String())
				}
				if m.SchemaVersion != 1 || m.Error.Code != tc.wantCode || m.Error.Message == "" {
					t.Fatalf("error = %+v, want code %s", m, tc.wantCode)
				}
			}
		})
	}
}

func TestSessionDiagnoseRegistered(t *testing.T) {
	cmd, _, err := newSessionCmd().Find([]string{"diagnose"})
	if err != nil || cmd.Name() != "diagnose" {
		t.Fatalf("diagnose not registered: %v", err)
	}
	if cmd.Flags().Lookup("json") == nil {
		t.Fatal("--json flag missing")
	}
}

func TestFetchReportUsesDaemonReportWhenSupported(t *testing.T) {
	want := diagnose.Build(diagResp(), diagnose.Inputs{Version: "daemon-v9", Now: time.Now()})
	raw, _ := want.MarshalJSON()
	got, err := fetchReport(context.Background(), diagSessionID,
		func(context.Context, string) ([]byte, error) { return raw, nil },
		func(context.Context) string { return "unused" },
		func(context.Context, string) (*bridgev1.GetSessionResponse, error) {
			t.Fatal("GetSession fallback must not run when the daemon supports DiagnoseSession")
			return nil, nil
		})
	if err != nil || got.BridgectlVersion != "daemon-v9" || got.SessionID != diagSessionID {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestFetchReportRejectsUnknownSchemaAndPropagatesErrors(t *testing.T) {
	noVersion := func(context.Context) string { return "" }
	noFallback := func(context.Context, string) (*bridgev1.GetSessionResponse, error) {
		t.Fatal("no fallback")
		return nil, nil
	}
	if _, err := fetchReport(context.Background(), diagSessionID,
		func(context.Context, string) ([]byte, error) { return []byte(`{"schema_version":2}`), nil }, noVersion, noFallback); err == nil {
		t.Fatal("a future schema version must be rejected, not rendered")
	}
	if _, err := fetchReport(context.Background(), diagSessionID,
		func(context.Context, string) ([]byte, error) { return nil, bridgeclient.ErrSessionNotFound }, noVersion, noFallback); !errors.Is(err, bridgeclient.ErrSessionNotFound) {
		t.Fatalf("NotFound must propagate without fallback, got %v", err)
	}
}

// TestFallbackReportNamesDaemonVersionNotCLI covers an upgrade without a
// daemon restart: the report must expose the daemon's (older) version.
func TestFallbackReportNamesDaemonVersionNotCLI(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	unimpl := func(context.Context, string) ([]byte, error) { return nil, status.Error(codes.Unimplemented, "old") }
	get := func(context.Context, string) (*bridgev1.GetSessionResponse, error) { return diagResp(), nil }
	for want, daemon := range map[string]func(context.Context) string{
		"v0.9.0-daemon":         func(context.Context) string { return "v0.9.0-daemon" },
		diagnose.UnknownVersion: func(context.Context) string { return diagnose.UnknownVersion },
	} {
		rep, err := fetchReport(context.Background(), diagSessionID, unimpl, daemon, get)
		if err != nil || rep.BridgectlVersion != want {
			t.Fatalf("bridgectl_version = %v (err %v), want %q (CLI is %q)", rep, err, want, version)
		}
	}
}
