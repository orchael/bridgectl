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
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

const diagSessionID = "11111111-1111-4111-8111-111111111111"

func diagGetter(resp *bridgev1.GetSessionResponse, err error) sessionGetter {
	return func(context.Context, string) (*bridgev1.GetSessionResponse, error) { return resp, err }
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
	if err := runSessionDiagnose(context.Background(), &out, diagSessionID, true, diagGetter(diagResp(), nil), time.Now()); err != nil {
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
	if err := runSessionDiagnose(context.Background(), &out, diagSessionID, false, diagGetter(diagResp(), nil), time.Now()); err != nil {
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
	if err := runSessionDiagnose(context.Background(), &out, diagSessionID, true, diagGetter(diagResp(), nil), time.Now()); err != nil {
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
		get      sessionGetter
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
				err := runSessionDiagnose(context.Background(), &out, tc.id, jsonOut, tc.get, time.Now())
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
