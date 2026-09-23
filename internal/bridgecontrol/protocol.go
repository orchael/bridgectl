// Package bridgecontrol implements the optional, local-first bridgectl side
// of Bridge's control-plane WebSocket protocol (protocol v1, see the merged
// orchael/bridge PR #16 / docs/control-plane-v1.md). It publishes presence
// and session lifecycle metadata to Bridge; it never carries PTY output,
// environment variables, or credentials, and a failure anywhere in this
// package must never affect local Supervisor/session operations.
package bridgecontrol

import (
	"encoding/json"
	"time"
)

// ProtocolVersion is the only control-plane protocol version this client
// speaks. Bridge rejects any other value on every message with a hard
// connection close (code "unsupported_protocol"); there is no negotiation
// or downgrade path.
const ProtocolVersion = 1

// Message types, exactly as defined by Bridge's control/protocol.go.
const (
	msgHello           = "hello"
	msgHelloAck        = "hello_ack"
	msgHeartbeat       = "heartbeat"
	msgSessionSnapshot = "session_snapshot"
	msgSessionStarted  = "session_started"
	msgSessionUpdated  = "session_updated"
	msgSessionStopped  = "session_stopped"
	msgAck             = "ack"
	msgError           = "error"
)

// Server-documented limits (control/protocol.go), mirrored here so the
// client fails fast locally instead of relying solely on the server to
// reject an oversized/invalid message.
const (
	maxMessageBytes     = 512 * 1024
	maxSnapshotSessions = 500
	maxBridgectlVersion = 64
)

// Error codes Bridge may send in an error payload.
const (
	errCodeRevisionGap         = "revision_gap"
	errCodeUnsupportedProtocol = "unsupported_protocol"
	errCodeInvalidMessage      = "invalid_message"
	errCodeInvalidRequest      = "invalid_request"
	errCodeSuperseded          = "superseded"
	errCodeHelloRequired       = "hello_required"
	errCodeAlreadyNegotiated   = "already_negotiated"
)

// Session status values Bridge accepts, exactly matching
// control.validSessionStatuses.
const (
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusAttached = "attached"
	StatusStopping = "stopping"
	StatusStopped  = "stopped"
	StatusFailed   = "failed"
	StatusUnknown  = "unknown"
)

// envelope is the wire shape of every control-plane message in both
// directions (control/protocol.go's envelope struct).
type envelope struct {
	ProtocolVersion int             `json:"protocol_version"`
	Type            string          `json:"type"`
	MessageID       string          `json:"message_id,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

type helloPayload struct {
	BridgectlVersion string          `json:"bridgectl_version"`
	Capabilities     json.RawMessage `json:"capabilities,omitempty"`
}

type helloAckPayload struct {
	InstallationID           string `json:"installation_id"`
	OrganizationID           string `json:"organization_id"`
	DisplayName              string `json:"display_name"`
	ProtocolVersion          int    `json:"protocol_version"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
}

// sessionPayload is both the element type of a session_snapshot's Sessions
// array and the payload of session_started/session_updated/session_stopped.
type sessionPayload struct {
	SessionID  string    `json:"session_id"`
	Provider   string    `json:"provider,omitempty"`
	ProjectID  string    `json:"project_id,omitempty"`
	Status     string    `json:"status"`
	Revision   int64     `json:"revision"`
	OccurredAt time.Time `json:"occurred_at"`
}

// sessionSnapshotPayload always marshals Sessions as a concrete (possibly
// empty) JSON array, never omitted or null: Bridge requires an explicit
// "sessions":[] to report zero active sessions and rejects a missing/null
// field as malformed.
type sessionSnapshotPayload struct {
	Sessions []sessionPayload `json:"sessions"`
}

type ackPayload struct {
	MessageID string `json:"message_id,omitempty"`
	Result    string `json:"result"`
}

type errorPayload struct {
	MessageID         string `json:"message_id,omitempty"`
	Code              string `json:"code"`
	Message           string `json:"message"`
	ReconcileRequired bool   `json:"reconcile_required,omitempty"`
}
