package diagnose

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
)

var update = flag.Bool("update", false, "rewrite golden files")

var (
	t0  = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	now = t0.Add(time.Hour)
)

func ptr[T any](v T) *T { return &v }

func baseResp() *bridgev1.GetSessionResponse {
	return &bridgev1.GetSessionResponse{
		SessionId: "11111111-1111-4111-8111-111111111111",
		ProjectId: "proj",
		Provider:  "claude",
		Status:    bridgev1.SessionStatus_SESSION_STATUS_RUNNING,
		CreatedAt: timestamppb.New(t0),
	}
}

func fullResp() *bridgev1.GetSessionResponse {
	r := baseResp()
	r.ActiveWriterClientId = "client-secret-id"
	r.ObserverCount = 2
	r.Interaction = &bridgev1.SessionInteraction{
		State:          "waiting_for_approval",
		Revision:       7,
		UpdatedAt:      timestamppb.New(t0.Add(time.Minute)),
		LastActivityAt: timestamppb.New(t0.Add(2 * time.Minute)),
		Source:         "codex-app-server",
		PendingRequest: &bridgev1.PendingInteractionRequest{Id: "req-1", Type: "approval", Kind: "command", Summary: "Run command: go test"},
		Capability: &bridgev1.InteractionCapability{
			InteractionStateSupported: true, ApprovalStateSupported: true, PendingSummarySupported: true,
			RemoteResponseSupported: true, StructuredApprovalSupported: true,
		},
	}
	return r
}

func connectedControl() Inputs {
	return Inputs{
		Version:                 "v1.2.3",
		Now:                     now,
		Control:                 ControlInput{Configured: true, Status: &bridgecontrol.Status{State: bridgecontrol.StateConnected, UpdatedAt: now.Add(-10 * time.Second), LastConnectedAt: now.Add(-10 * time.Second)}},
		LifecycleRevisionWire:   ptr(int64(4)),
		InteractionRevisionWire: ptr(int64(3)),
	}
}

func marshal(t *testing.T, r *Report) []byte {
	t.Helper()
	b, err := r.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	return b
}

func TestGoldenSchemaV1(t *testing.T) {
	got := marshal(t, Build(fullResp(), connectedControl()))
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	path := filepath.Join("testdata", "report_v1.golden.json")
	if *update {
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Fatalf("golden mismatch\n--- want\n%s\n--- got\n%s", want, pretty.Bytes())
	}
}

func TestSchemaVersionIsFirstField(t *testing.T) {
	b := marshal(t, Build(baseResp(), Inputs{Now: now}))
	if !bytes.HasPrefix(b, []byte(`{"schema_version":1,`)) {
		t.Fatalf("schema_version must be the first field: %s", b)
	}
	eb := marshalError(t, CodeSessionNotFound)
	if !bytes.HasPrefix(eb, []byte(`{"schema_version":1,`)) {
		t.Fatalf("error schema_version must be first: %s", eb)
	}
}

func marshalError(t *testing.T, code ErrorCode) []byte {
	t.Helper()
	b, err := json.Marshal(NewErrorReport(code))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDeterministicSerialization(t *testing.T) {
	a := marshal(t, Build(fullResp(), connectedControl()))
	for i := 0; i < 20; i++ {
		if b := marshal(t, Build(fullResp(), connectedControl())); !bytes.Equal(a, b) {
			t.Fatalf("serialization differs between runs:\n%s\n%s", a, b)
		}
	}
}

func TestRuntimeStatusAgreesWithPublicAPI(t *testing.T) {
	cases := []struct {
		in   bridgev1.SessionStatus
		want string
	}{
		{bridgev1.SessionStatus_SESSION_STATUS_STARTING, "starting"},
		{bridgev1.SessionStatus_SESSION_STATUS_RUNNING, "running"},
		{bridgev1.SessionStatus_SESSION_STATUS_ATTACHED, "attached"},
		{bridgev1.SessionStatus_SESSION_STATUS_STOPPING, "stopping"},
		{bridgev1.SessionStatus_SESSION_STATUS_STOPPED, "stopped"},
		{bridgev1.SessionStatus_SESSION_STATUS_FAILED, "failed"},
		{bridgev1.SessionStatus_SESSION_STATUS_UNSPECIFIED, "unknown"},
	}
	for _, tc := range cases {
		r := baseResp()
		r.Status = tc.in
		if got := Build(r, Inputs{Now: now}).Status; got != tc.want {
			t.Errorf("status %v = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExitCodeOnlyWhenRecorded(t *testing.T) {
	r := baseResp()
	r.Status = bridgev1.SessionStatus_SESSION_STATUS_FAILED
	r.ExitCode = 0
	r.ExitRecorded = false
	if got := Build(r, Inputs{Now: now}).ExitCode; got != nil {
		t.Fatalf("exit_code = %v, want null when not recorded", *got)
	}
	r.ExitRecorded, r.ExitCode = true, 137
	if got := Build(r, Inputs{Now: now}).ExitCode; got == nil || *got != 137 {
		t.Fatalf("exit_code = %v, want 137", got)
	}
	r.ExitCode = 0
	if got := Build(r, Inputs{Now: now}).ExitCode; got == nil || *got != 0 {
		t.Fatalf("recorded exit 0 must be reported as 0, got %v", got)
	}
}

func TestInteractionStatesPassThrough(t *testing.T) {
	for _, st := range []string{"working", "waiting_for_input", "waiting_for_approval", "idle", "unknown"} {
		r := baseResp()
		r.Interaction = &bridgev1.SessionInteraction{State: st, Capability: &bridgev1.InteractionCapability{InteractionStateSupported: true}}
		if got := Build(r, Inputs{Now: now}).InteractionState; got != st {
			t.Errorf("interaction_state = %q, want %q", got, st)
		}
	}
}

func TestInteractionStateIsNeverInferredFromRuntime(t *testing.T) {
	// A running, attached, recently active session with no interaction
	// report must stay unknown — never "working" or "idle".
	r := baseResp()
	r.Attached, r.ActiveWriterClientId = true, "c"
	r.LastSeq = 9000
	got := Build(r, Inputs{Now: now})
	if got.InteractionState != "unknown" {
		t.Fatalf("interaction_state = %q, want unknown", got.InteractionState)
	}
}

func TestUnknownVersusUnsupportedCapability(t *testing.T) {
	// Older server / no interaction message: capability is *unknown* (null).
	unknown := Build(baseResp(), Inputs{Now: now})
	if unknown.InteractionCapability != nil {
		t.Fatalf("capability must be null when the server reported no interaction block, got %+v", unknown.InteractionCapability)
	}
	if unknown.InteractionState != "unknown" {
		t.Fatalf("state = %q", unknown.InteractionState)
	}

	// Provider reported an all-false capability: explicitly *unsupported*.
	r := baseResp()
	r.Interaction = &bridgev1.SessionInteraction{State: "unknown", Capability: &bridgev1.InteractionCapability{}}
	unsupported := Build(r, Inputs{Now: now})
	if unsupported.InteractionCapability == nil || *unsupported.InteractionCapability != (Capability{}) {
		t.Fatalf("capability = %+v, want explicit all-false", unsupported.InteractionCapability)
	}

	ub, sb := marshal(t, unknown), marshal(t, unsupported)
	if !strings.Contains(string(ub), `"interaction_capability":null`) {
		t.Errorf("unknown capability not serialized as null: %s", ub)
	}
	if !strings.Contains(string(sb), `"interaction_state_supported":false`) {
		t.Errorf("unsupported capability flags missing: %s", sb)
	}
}

func TestCapabilityFlagsMapped(t *testing.T) {
	r := baseResp()
	r.Interaction = &bridgev1.SessionInteraction{Capability: &bridgev1.InteractionCapability{
		InteractionStateSupported: true, ApprovalStateSupported: false, PendingSummarySupported: true,
		RemoteResponseSupported: false, StructuredApprovalSupported: true,
	}}
	c := Build(r, Inputs{Now: now}).InteractionCapability
	want := Capability{InteractionStateSupported: true, PendingSummarySupported: true, StructuredApprovalSupported: true}
	if c == nil || *c != want {
		t.Fatalf("capability = %+v, want %+v", c, want)
	}
}

func TestPendingRequestIdentityAndSummary(t *testing.T) {
	rep := Build(fullResp(), Inputs{Now: now})
	p := rep.PendingRequest
	if p == nil || p.ID != "req-1" || p.Type != "approval" {
		t.Fatalf("pending = %+v", p)
	}
	if !p.SummaryAvailable {
		t.Fatal("summary_available must be true when the provider supplied a summary")
	}

	// No pending request: null, not an empty object.
	r := fullResp()
	r.Interaction.PendingRequest = nil
	r.Interaction.State = "working"
	if Build(r, Inputs{Now: now}).PendingRequest != nil {
		t.Fatal("pending_request must be null when none is pending")
	}
}

func TestPendingSummaryTextIsNeverIncluded(t *testing.T) {
	const secret = "Run once in /home/dev/secret-repo: curl -H 'Authorization: Bearer sk-live-123' https://x"
	r := fullResp()
	r.Interaction.PendingRequest.Summary = secret
	rep := Build(r, Inputs{Now: now})
	var human bytes.Buffer
	Render(&human, rep)
	for where, out := range map[string]string{"json": string(marshal(t, rep)), "human": human.String()} {
		for _, frag := range []string{"/home/dev", "secret-repo", "sk-live-123", "Bearer", "curl"} {
			if strings.Contains(out, frag) {
				t.Errorf("%s leaked summary fragment %q: %s", where, frag, out)
			}
		}
	}
}

func TestPendingKindIsFixedVocabulary(t *testing.T) {
	cases := map[string]string{
		"command": "command", "file_change": "file_change", "tool": "tool", "question": "question", "other": "other",
		"": "unknown", "rm -rf /home/dev": "other", "Run once in /home/dev: curl": "other",
	}
	for in, want := range cases {
		r := fullResp()
		r.Interaction.PendingRequest.Kind = in
		if got := Build(r, Inputs{Now: now}).PendingRequest.Kind; got != want {
			t.Errorf("kind %q -> %q, want %q", in, got, want)
		}
	}
}

func TestSummaryAvailableFlag(t *testing.T) {
	r := fullResp()
	r.Interaction.Capability.PendingSummarySupported = false
	if Build(r, Inputs{Now: now}).PendingRequest.SummaryAvailable {
		t.Fatal("summary_available must be false when provider does not declare support")
	}
	r.Interaction.Capability.PendingSummarySupported = true
	r.Interaction.PendingRequest.Summary = "  "
	if Build(r, Inputs{Now: now}).PendingRequest.SummaryAvailable {
		t.Fatal("blank summary is not available")
	}
}

func TestPendingIDIsBounded(t *testing.T) {
	r := fullResp()
	r.Interaction.PendingRequest.Id = strings.Repeat("x", 5000)
	if n := len(Build(r, Inputs{Now: now}).PendingRequest.ID); n > MaxIDRunes {
		t.Fatalf("pending id length %d > %d", n, MaxIDRunes)
	}
}

func TestInteractionRevisionChangesAreReflected(t *testing.T) {
	r := fullResp()
	a := Build(r, Inputs{Now: now}).InteractionRevisionLocal
	r.Interaction.Revision++
	b := Build(r, Inputs{Now: now}).InteractionRevisionLocal
	if a == nil || b == nil || *b != *a+1 {
		t.Fatalf("local interaction revision a=%v b=%v", a, b)
	}
}

func TestRevisionsAreSeparateAndNullWhenUnknown(t *testing.T) {
	rep := Build(fullResp(), connectedControl())
	if *rep.LifecycleRevisionWire != 4 || *rep.InteractionRevisionWire != 3 || *rep.InteractionRevisionLocal != 7 {
		t.Fatalf("revisions = lifecycle %v wire-interaction %v local-interaction %v",
			*rep.LifecycleRevisionWire, *rep.InteractionRevisionWire, *rep.InteractionRevisionLocal)
	}
	none := Build(fullResp(), Inputs{Now: now})
	if none.LifecycleRevisionWire != nil || none.InteractionRevisionWire != nil {
		t.Fatal("wire revisions must be null when the control client has none for the session")
	}
	if none.InteractionRevisionLocal == nil {
		t.Fatal("local revision must still be reported")
	}
	// No interaction block at all: local revision unknown, not 0.
	if Build(baseResp(), Inputs{Now: now}).InteractionRevisionLocal != nil {
		t.Fatal("local interaction revision must be null when no interaction block was reported")
	}
}

func TestMissingOptionalTimestampsAreNull(t *testing.T) {
	r := baseResp()
	r.CreatedAt = nil
	r.Interaction = &bridgev1.SessionInteraction{State: "unknown", Capability: &bridgev1.InteractionCapability{}}
	rep := Build(r, Inputs{Now: now})
	if rep.CreatedAt != nil || rep.StoppedAt != nil || rep.InteractionUpdatedAt != nil || rep.InteractionLastReportAt != nil {
		t.Fatalf("expected all timestamps null: %+v", rep)
	}
	b := string(marshal(t, rep))
	for _, k := range []string{"created_at", "stopped_at", "interaction_updated_at", "interaction_last_report_at"} {
		if !strings.Contains(b, `"`+k+`":null`) {
			t.Errorf("%s not serialized as null: %s", k, b)
		}
	}
}

func TestTimestampsAreUTCRFC3339(t *testing.T) {
	r := fullResp()
	r.StoppedAt = timestamppb.New(t0.Add(3 * time.Hour).In(time.FixedZone("x", 3600)))
	rep := Build(r, Inputs{Now: now})
	if *rep.CreatedAt != "2026-03-01T10:00:00Z" {
		t.Fatalf("created_at = %s", *rep.CreatedAt)
	}
	if *rep.StoppedAt != "2026-03-01T13:00:00Z" {
		t.Fatalf("stopped_at = %s", *rep.StoppedAt)
	}
	if *rep.InteractionUpdatedAt != "2026-03-01T10:01:00Z" || *rep.InteractionLastReportAt != "2026-03-01T10:02:00Z" {
		t.Fatalf("interaction timestamps = %s %s", *rep.InteractionUpdatedAt, *rep.InteractionLastReportAt)
	}
}

func TestActiveWriterIsBooleanOnly(t *testing.T) {
	r := baseResp()
	if Build(r, Inputs{Now: now}).ActiveWriter {
		t.Fatal("no writer expected")
	}
	r.ActiveWriterClientId = "writer-client-123"
	r.ObserverCount = 3
	rep := Build(r, Inputs{Now: now})
	if !rep.ActiveWriter || rep.ObserverCount != 3 {
		t.Fatalf("writer=%v observers=%d", rep.ActiveWriter, rep.ObserverCount)
	}
	if strings.Contains(string(marshal(t, rep)), "writer-client-123") {
		t.Fatal("active writer client ID leaked into diagnostics")
	}
}

func TestControlStatus(t *testing.T) {
	fresh := now.Add(-5 * time.Second)
	old := now.Add(-bridgecontrol.StatusStaleAfter - time.Second)
	cases := []struct {
		name      string
		in        ControlInput
		wantState string
		wantStale bool
		wantTime  bool
	}{
		{"not configured", ControlInput{}, "unknown", false, false},
		{"configured, no status file", ControlInput{Configured: true}, "unknown", false, false},
		{"connected fresh", ControlInput{Configured: true, Status: &bridgecontrol.Status{State: bridgecontrol.StateConnected, UpdatedAt: fresh}}, "connected", false, true},
		{"connected stale", ControlInput{Configured: true, Status: &bridgecontrol.Status{State: bridgecontrol.StateConnected, UpdatedAt: old}}, "connected", true, true},
		{"disconnected", ControlInput{Configured: true, Status: &bridgecontrol.Status{State: bridgecontrol.StateDisconnected, UpdatedAt: fresh}}, "disconnected", false, true},
		{"auth rejected", ControlInput{Configured: true, Status: &bridgecontrol.Status{State: bridgecontrol.StateAuthRejected, UpdatedAt: fresh}}, "auth_rejected", false, true},
		{"not provisioned", ControlInput{Configured: true, Status: &bridgecontrol.Status{State: bridgecontrol.StateNotProvisioned, UpdatedAt: fresh}}, "not_provisioned", false, true},
		{"unrecognized state is unknown", ControlInput{Configured: true, Status: &bridgecontrol.Status{State: "bogus-\x1b[0m", UpdatedAt: fresh}}, "unknown", false, true},
		{"zero time", ControlInput{Configured: true, Status: &bridgecontrol.Status{State: bridgecontrol.StateConnecting}}, "connecting", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Build(baseResp(), Inputs{Now: now, Control: tc.in}).Control
			if c.State != tc.wantState || c.Stale != tc.wantStale || (c.UpdatedAt != nil) != tc.wantTime {
				t.Fatalf("control = %+v (updated_at set=%v), want state=%s stale=%v time=%v", c, c.UpdatedAt != nil, tc.wantState, tc.wantStale, tc.wantTime)
			}
		})
	}
}

func TestControlLastConnectedAt(t *testing.T) {
	last := now.Add(-3 * time.Hour)
	in := Inputs{Now: now, Control: ControlInput{Configured: true, Status: &bridgecontrol.Status{
		State: bridgecontrol.StateUnavailable, UpdatedAt: now.Add(-time.Second), LastConnectedAt: last}}}
	c := Build(baseResp(), in).Control
	if c.LastConnectedAt == nil || *c.LastConnectedAt != "2026-03-01T08:00:00Z" {
		t.Fatalf("last_connected_at = %v", c.LastConnectedAt)
	}
	in.Control.Status.LastConnectedAt = time.Time{}
	if Build(baseResp(), in).Control.LastConnectedAt != nil {
		t.Fatal("last_connected_at must be null when never recorded")
	}
}

func TestControlLastErrorIsNeverIncluded(t *testing.T) {
	in := Inputs{Now: now, Control: ControlInput{Configured: true, Status: &bridgecontrol.Status{
		State: bridgecontrol.StateUnavailable, UpdatedAt: now, LastError: "dial tcp: token=sekrit-token-value", InstallationID: "inst-secret-1",
	}}}
	b := string(marshal(t, Build(baseResp(), in)))
	for _, s := range []string{"sekrit-token-value", "inst-secret-1", "dial tcp"} {
		if strings.Contains(b, s) {
			t.Fatalf("control status leaked %q: %s", s, b)
		}
	}
}

func TestLoadInputs(t *testing.T) {
	dir := t.TempDir()
	const id = "11111111-1111-4111-8111-111111111111"

	// Nothing on disk: control not configured, revisions null. Must not error.
	in := LoadInputs(dir, id, "v9", now)
	if in.Control.Configured || in.LifecycleRevisionWire != nil || in.InteractionRevisionWire != nil || in.Version != "v9" {
		t.Fatalf("empty dir inputs = %+v", in)
	}

	if err := bridgecontrol.WriteStatus(filepath.Join(dir, bridgecontrol.StatusFileName), bridgecontrol.Status{State: bridgecontrol.StateConnected, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	rev := filepath.Join(dir, bridgecontrol.RevisionFileName)
	writeJSON(t, rev, map[string]int64{id: 5, "other": 9})
	writeJSON(t, bridgecontrol.InteractionRevisionPath(rev), map[string]int64{id: 2})

	in = LoadInputs(dir, id, "v9", now)
	if !in.Control.Configured || in.Control.Status == nil || in.Control.Status.State != bridgecontrol.StateConnected {
		t.Fatalf("control = %+v", in.Control)
	}
	if in.LifecycleRevisionWire == nil || *in.LifecycleRevisionWire != 5 || in.InteractionRevisionWire == nil || *in.InteractionRevisionWire != 2 {
		t.Fatalf("revisions = %v %v", in.LifecycleRevisionWire, in.InteractionRevisionWire)
	}

	// Session the control client does not know: null.
	in = LoadInputs(dir, "22222222-2222-4222-8222-222222222222", "v9", now)
	if in.LifecycleRevisionWire != nil || in.InteractionRevisionWire != nil {
		t.Fatalf("unknown session must have null wire revisions: %+v", in)
	}

	// Corrupt status file: unreadable → unknown, never an error or crash.
	if err := os.WriteFile(filepath.Join(dir, bridgecontrol.StatusFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	in = LoadInputs(dir, id, "v9", now)
	if got := Build(baseResp(), in).Control.State; got != "unknown" {
		t.Fatalf("corrupt status state = %q", got)
	}
}

func TestLoadInputsDoesNotCreateOrModifyFiles(t *testing.T) {
	dir := t.TempDir()
	_ = LoadInputs(dir, "11111111-1111-4111-8111-111111111111", "v", now)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("LoadInputs wrote files: %v", entries)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestErrorReport(t *testing.T) {
	for _, code := range []ErrorCode{CodeInvalidSessionID, CodeSessionNotFound, CodeServerUnavailable, CodeInternal} {
		var got map[string]any
		if err := json.Unmarshal(marshalError(t, code), &got); err != nil {
			t.Fatal(err)
		}
		if got["schema_version"] != float64(1) {
			t.Errorf("%s: schema_version = %v", code, got["schema_version"])
		}
		e, ok := got["error"].(map[string]any)
		if !ok || e["code"] != string(code) || e["message"] == "" {
			t.Errorf("%s: error = %v", code, got["error"])
		}
		if _, has := got["session_id"]; has {
			t.Errorf("%s: error report must not echo caller-supplied input", code)
		}
	}
}

// ---- Security ----

// allowedStringFields enumerates every string-typed JSON field a Report may
// carry. Adding a string field to the model fails this test until the field
// is reviewed for privacy and added here deliberately.
var allowedStringFields = map[string]bool{
	"bridgectl_version":          true, // build-time constant
	"session_id":                 true, // caller-supplied UUID, validated
	"provider":                   true, // registry provider ID
	"project_id":                 true,
	"status":                     true, // enum
	"created_at":                 true, // timestamp
	"stopped_at":                 true, // timestamp
	"interaction_state":          true, // enum
	"interaction_updated_at":     true, // timestamp
	"interaction_last_report_at": true, // timestamp
	"pending_request.id":         true, // provider request identity, bounded
	"pending_request.type":       true, // enum
	"pending_request.kind":       true, // normalized fixed vocabulary
	"control.state":              true, // enum
	"control.updated_at":         true, // timestamp
	"control.last_connected_at":  true, // timestamp
}

func TestReportHasNoUnreviewedStringFields(t *testing.T) {
	var walk func(prefix string, typ reflect.Type)
	walk = func(prefix string, typ reflect.Type) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				t.Errorf("%s.%s has no json tag", typ.Name(), f.Name)
				continue
			}
			full := name
			if prefix != "" {
				full = prefix + "." + name
			}
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.String:
				if !allowedStringFields[full] {
					t.Errorf("unreviewed string field %q in diagnostic model", full)
				}
			case reflect.Struct:
				walk(full, ft)
			case reflect.Slice, reflect.Map, reflect.Interface:
				t.Errorf("field %q has free-form type %s; diagnostics must be fixed-shape", full, ft)
			}
		}
	}
	walk("", reflect.TypeOf(Report{}))
}

// TestSecurityExclusions fills every sensitive-looking source field with a
// sentinel and proves none reaches JSON or the human renderer.
func TestSecurityExclusions(t *testing.T) {
	sentinels := map[string]string{
		"pty output":       "PTYOUT-\x1b[31mred",
		"transcript":       "TRANSCRIPT-assistant said hello",
		"prompt":           "PROMPT-fix my bug",
		"response":         "RESPONSE-sure",
		"env":              "ENV-AWS_SECRET_ACCESS_KEY=abc",
		"token":            "TOKEN-brc_A1b2C3",
		"oauth":            "OAUTH-refresh-xyz",
		"filesystem":       "/home/dev/secret-repo/FSCONTENT",
		"cot":              "COT-let me think step by step",
		"provider payload": "PAYLOAD-{\"arbitrary\":true}",
		"client id":        "CLIENTID-abc",
	}
	r := fullResp()
	r.Error = sentinels["pty output"] + sentinels["env"] + sentinels["token"]
	r.RepoPath = sentinels["filesystem"]
	r.AttachedClientId = sentinels["client id"]
	r.ActiveWriterClientId = sentinels["client id"]
	r.Interaction.Source = sentinels["provider payload"]
	r.Interaction.PendingRequest.Summary = "Run once in /home/dev/SECRET-repo: curl SECRET-token"
	in := connectedControl()
	in.Control.Status.LastError = sentinels["oauth"] + sentinels["cot"]
	in.Control.Status.InstallationID = sentinels["prompt"]

	rep := Build(r, in)
	var human bytes.Buffer
	Render(&human, rep)
	outputs := map[string]string{"json": string(marshal(t, rep)), "human": human.String()}

	for where, out := range outputs {
		for name, s := range sentinels {
			if strings.Contains(out, s) || strings.Contains(out, strings.SplitN(s, "-", 2)[0]+"-") {
				t.Errorf("%s output leaked %s sentinel %q:\n%s", where, name, s, out)
			}
		}
		if strings.Contains(out, "/home/dev") {
			t.Errorf("%s output leaked a filesystem path", where)
		}
	}
}

func TestForbiddenKeysAbsentFromSchema(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(marshal(t, Build(fullResp(), connectedControl())), &m); err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"output", "pty", "transcript", "prompt", "env", "environment", "token", "credential",
		"oauth", "secret", "password", "repo_path", "path", "reasoning", "thinking", "payload", "error", "last_error", "client_id", "installation_id"}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		mm, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, child := range mm {
			for _, f := range forbidden {
				if strings.Contains(k, f) {
					t.Errorf("forbidden-looking key %q in schema at %s", k, prefix)
				}
			}
			walk(prefix+"."+k, child)
		}
	}
	walk("", m)
}

// ---- Human renderer ----

func TestRenderIsPureFunctionOfReport(t *testing.T) {
	// A hand-built Report that no Supervisor could produce: the renderer must
	// show it as-is, proving it recalculates nothing.
	rep := &Report{
		SchemaVersion:    SchemaVersion,
		BridgectlVersion: "vX",
		SessionID:        "sid",
		Provider:         "prov",
		ProjectID:        "pid",
		Status:           "stopped",
		InteractionState: "waiting_for_input",
		Control:          Control{State: "connected", Stale: true},
	}
	var b bytes.Buffer
	Render(&b, rep)
	out := b.String()
	for _, want := range []string{"sid", "prov", "pid", "stopped", "waiting_for_input", "vX", "connected", "stale"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderShowsExplicitUnknowns(t *testing.T) {
	var b bytes.Buffer
	Render(&b, Build(baseResp(), Inputs{Now: now}))
	out := b.String()
	for _, want := range []string{"unknown", "not reported"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderGolden(t *testing.T) {
	var b bytes.Buffer
	Render(&b, Build(fullResp(), connectedControl()))
	path := filepath.Join("testdata", "report_v1.golden.txt")
	if *update {
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if !bytes.Equal(want, b.Bytes()) {
		t.Fatalf("render golden mismatch\n--- want\n%s\n--- got\n%s", want, b.Bytes())
	}
}
