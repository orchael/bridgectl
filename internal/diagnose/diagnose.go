// Package diagnose builds the bounded, machine-readable diagnostic snapshot
// reported by `bridgectl session diagnose`.
//
// The snapshot answers "what does bridgectl believe about this session right
// now". It is derived only from the Supervisor's public session API
// (bridgev1.GetSessionResponse) plus two small local control-client files; it
// never reads telemetry, Bridge, terminal output, logs or the filesystem of
// the session, and it never infers interaction state. Report is the single
// canonical model: the JSON serializer and the human renderer both consume
// it, and neither recalculates anything.
//
// The shape is a published contract (schema_version 1). Every string-typed
// field is enumerated in the privacy allowlist test, so adding one is a
// deliberate, reviewed act. See docs/agent-diagnostics.md.
package diagnose

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
)

// SchemaVersion is the diagnostic JSON schema version. Field names, types and
// enum values of a published version never change; incompatible changes bump
// this number.
const SchemaVersion = 1

const (
	// MaxIDRunes bounds the provider-supplied pending request ID.
	MaxIDRunes = 128
)

// Report is the schema-v1 diagnostic snapshot. Field order is the
// serialization order; schema_version is always first. Optional values are
// pointers and serialize as explicit null — never omitted — so "unknown" is
// always distinguishable from "false"/"0".
type Report struct {
	SchemaVersion    int    `json:"schema_version"`
	BridgectlVersion string `json:"bridgectl_version"`

	SessionID string  `json:"session_id"`
	Provider  string  `json:"provider"`
	ProjectID string  `json:"project_id"`
	Status    string  `json:"status"`
	ExitCode  *int32  `json:"exit_code"`
	CreatedAt *string `json:"created_at"`
	StoppedAt *string `json:"stopped_at"`

	InteractionState        string      `json:"interaction_state"`
	InteractionCapability   *Capability `json:"interaction_capability"`
	InteractionUpdatedAt    *string     `json:"interaction_updated_at"`
	InteractionLastReportAt *string     `json:"interaction_last_report_at"`
	PendingRequest          *Pending    `json:"pending_request"`

	LifecycleRevisionWire    *int64  `json:"lifecycle_revision_wire"`
	InteractionRevisionWire  *int64  `json:"interaction_revision_wire"`
	InteractionRevisionLocal *uint64 `json:"interaction_revision_local"`

	ActiveWriter  bool `json:"active_writer"`
	ObserverCount int  `json:"observer_count"`

	Control Control `json:"control"`
}

// Capability mirrors bridge.InteractionCapabilities. A null
// interaction_capability means the capability is unknown (the server reported
// no interaction block); an all-false object means the provider explicitly
// supports nothing.
type Capability struct {
	InteractionStateSupported   bool `json:"interaction_state_supported"`
	ApprovalStateSupported      bool `json:"approval_state_supported"`
	PendingSummarySupported     bool `json:"pending_summary_supported"`
	RemoteResponseSupported     bool `json:"remote_response_supported"`
	StructuredApprovalSupported bool `json:"structured_approval_supported"`
}

// Pending identifies the request the session is blocked on.
type Pending struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Kind is the provider-assigned category: command, file_change, tool,
	// question, other, or unknown when the provider did not classify it. Any
	// other value from a provider is reported as other, so this can never
	// carry free text.
	Kind string `json:"kind"`
	// SummaryAvailable reports that the provider supplied a pending summary.
	// The text itself is never included: provider summaries (e.g. Codex
	// approvals) can embed working directories, full commands and anything
	// typed into them, which no sanitizer can make safe.
	SummaryAvailable bool `json:"summary_available"`
}

// Control is the local Bridge control connection status, read from the file
// the daemon persists. It never reflects Bridge reachability measured now.
type Control struct {
	// State is one of the bridgecontrol.State values, or "unknown" when the
	// status path is not configured, unreadable, or holds an unrecognized value.
	State string `json:"state"`
	// UpdatedAt is when the status last changed (or, while connected, last
	// heartbeated).
	UpdatedAt *string `json:"updated_at"`
	// LastConnectedAt is the last time the control client was observed
	// connected, or null if never recorded (including status files written
	// by older versions).
	LastConnectedAt *string `json:"last_connected_at"`
	// Stale is true when UpdatedAt is older than bridgecontrol.StatusStaleAfter.
	// A stale "connected" entry means no live client is behind it.
	Stale bool `json:"stale"`
}

// ControlInput is the raw control-client status. Configured is false when no
// control files exist; Status is nil when the file is missing or unreadable.
type ControlInput struct {
	Configured bool
	Status     *bridgecontrol.Status
}

// Inputs is everything Build needs besides the session itself. It is plain
// data so Build is a pure function.
type Inputs struct {
	Version                 string
	Now                     time.Time
	Control                 ControlInput
	LifecycleRevisionWire   *int64
	InteractionRevisionWire *int64
}

// LoadInputs reads the control client's persisted status and revision
// counters from stateDir. It is read-only (it never creates or modifies a
// file) and never fails: anything missing or unreadable becomes unknown/null,
// so diagnostics work identically when Bridge is not enrolled or unavailable.
func LoadInputs(stateDir, sessionID, version string, now time.Time) Inputs {
	in := Inputs{Version: version, Now: now}

	statusPath := filepath.Join(stateDir, bridgecontrol.StatusFileName)
	revPath := filepath.Join(stateDir, bridgecontrol.RevisionFileName)
	intPath := bridgecontrol.InteractionRevisionPath(revPath)

	if st, err := bridgecontrol.ReadStatus(statusPath); err == nil {
		in.Control = ControlInput{Configured: true, Status: st}
	} else if !errors.Is(err, os.ErrNotExist) {
		in.Control = ControlInput{Configured: true}
	}

	// NewRevisionStore only reads; Current does not write.
	if exists(revPath) {
		if n := bridgecontrol.NewRevisionStore(revPath).Current(sessionID); n > 0 {
			in.LifecycleRevisionWire = &n
		}
	}
	if exists(intPath) {
		if n := bridgecontrol.NewRevisionStore(intPath).Current(sessionID); n > 0 {
			in.InteractionRevisionWire = &n
		}
	}
	if !in.Control.Configured && (exists(revPath) || exists(intPath)) {
		in.Control.Configured = true
	}
	return in
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Build converts the Supervisor's public session view into a Report. It never
// derives interaction state from runtime status, timestamps or output.
func Build(resp *bridgev1.GetSessionResponse, in Inputs) *Report {
	r := &Report{
		SchemaVersion:    SchemaVersion,
		BridgectlVersion: in.Version,
		SessionID:        resp.GetSessionId(),
		Provider:         resp.GetProvider(),
		ProjectID:        resp.GetProjectId(),
		Status:           statusString(resp.GetStatus()),
		CreatedAt:        formatTime(resp.GetCreatedAt().AsTime(), resp.GetCreatedAt() != nil),
		StoppedAt:        formatTime(resp.GetStoppedAt().AsTime(), resp.GetStoppedAt() != nil),

		InteractionState:        "unknown",
		LifecycleRevisionWire:   in.LifecycleRevisionWire,
		InteractionRevisionWire: in.InteractionRevisionWire,

		ActiveWriter:  resp.GetActiveWriterClientId() != "",
		ObserverCount: int(resp.GetObserverCount()),
		Control:       buildControl(in),
	}
	if resp.GetExitRecorded() {
		code := resp.GetExitCode()
		r.ExitCode = &code
	}

	ia := resp.GetInteraction()
	if ia == nil {
		return r
	}
	if s := ia.GetState(); s != "" {
		r.InteractionState = s
	}
	rev := ia.GetRevision()
	r.InteractionRevisionLocal = &rev
	r.InteractionUpdatedAt = formatTime(ia.GetUpdatedAt().AsTime(), ia.GetUpdatedAt() != nil)
	r.InteractionLastReportAt = formatTime(ia.GetLastActivityAt().AsTime(), ia.GetLastActivityAt() != nil)

	var caps Capability
	if c := ia.GetCapability(); c != nil {
		caps = Capability{
			InteractionStateSupported:   c.GetInteractionStateSupported(),
			ApprovalStateSupported:      c.GetApprovalStateSupported(),
			PendingSummarySupported:     c.GetPendingSummarySupported(),
			RemoteResponseSupported:     c.GetRemoteResponseSupported(),
			StructuredApprovalSupported: c.GetStructuredApprovalSupported(),
		}
		r.InteractionCapability = &caps
	}

	if p := ia.GetPendingRequest(); p != nil {
		pending := &Pending{ID: truncate(sanitize(p.GetId()), MaxIDRunes), Type: p.GetType(), Kind: normalizeKind(p.GetKind())}
		pending.SummaryAvailable = caps.PendingSummarySupported && strings.TrimSpace(p.GetSummary()) != ""
		r.PendingRequest = pending
	}
	return r
}

func normalizeKind(k string) string {
	switch k {
	case "":
		return "unknown"
	case "command", "file_change", "tool", "question", "other":
		return k
	default:
		return "other"
	}
}

func buildControl(in Inputs) Control {
	c := Control{State: "unknown"}
	st := in.Control.Status
	if !in.Control.Configured || st == nil {
		return c
	}
	switch st.State {
	case bridgecontrol.StateNotProvisioned, bridgecontrol.StateConnecting, bridgecontrol.StateConnected,
		bridgecontrol.StateDisconnected, bridgecontrol.StateAuthRejected, bridgecontrol.StateUnavailable:
		c.State = string(st.State)
	}
	if !st.UpdatedAt.IsZero() {
		c.UpdatedAt = formatTime(st.UpdatedAt, true)
		c.Stale = in.Now.Sub(st.UpdatedAt) > bridgecontrol.StatusStaleAfter
	}
	c.LastConnectedAt = formatTime(st.LastConnectedAt, true)
	return c
}

func statusString(s bridgev1.SessionStatus) string {
	switch s {
	case bridgev1.SessionStatus_SESSION_STATUS_STARTING:
		return "starting"
	case bridgev1.SessionStatus_SESSION_STATUS_RUNNING:
		return "running"
	case bridgev1.SessionStatus_SESSION_STATUS_ATTACHED:
		return "attached"
	case bridgev1.SessionStatus_SESSION_STATUS_STOPPING:
		return "stopping"
	case bridgev1.SessionStatus_SESSION_STATUS_STOPPED:
		return "stopped"
	case bridgev1.SessionStatus_SESSION_STATUS_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}

func formatTime(t time.Time, present bool) *string {
	if !present || t.IsZero() || t.Unix() == 0 {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// sanitize replaces control characters (newlines, escapes, ...) with spaces
// so a provider-supplied value cannot smuggle terminal escapes or multi-line
// content into diagnostics.
func sanitize(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

// MarshalJSON is the canonical serialization: compact, deterministic (struct
// field order), one document.
func (r *Report) MarshalJSON() ([]byte, error) {
	type plain Report // drop the method to avoid recursion
	return json.Marshal((*plain)(r))
}

// ErrorCode is a fixed-vocabulary failure code.
type ErrorCode string

const (
	CodeInvalidSessionID  ErrorCode = "invalid_session_id"
	CodeSessionNotFound   ErrorCode = "session_not_found"
	CodeServerUnavailable ErrorCode = "server_unavailable"
	CodeInternal          ErrorCode = "internal"
)

var errorMessages = map[ErrorCode]string{
	CodeInvalidSessionID:  "session ID is not a valid UUID",
	CodeSessionNotFound:   "no session with that ID is known to the local bridgectl server",
	CodeServerUnavailable: "the local bridgectl server is not running or not reachable",
	CodeInternal:          "the local bridgectl server could not report this session",
}

// ErrorReport is what `--json` prints on failure, in place of a Report. It
// never echoes caller-supplied input or server error text.
type ErrorReport struct {
	SchemaVersion int       `json:"schema_version"`
	Error         ErrorBody `json:"error"`
}

// ErrorBody is the code plus a fixed message for that code.
type ErrorBody struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// NewErrorReport builds the schema-v1 error document for code.
func NewErrorReport(code ErrorCode) *ErrorReport {
	return &ErrorReport{SchemaVersion: SchemaVersion, Error: ErrorBody{Code: code, Message: errorMessages[code]}}
}
