// Package codexapp adapts Codex app-server interaction state and request-bound
// input responses. Proxy observes the real TUI owner connection and answers
// only an explicitly supported outstanding input request. It never starts a
// turn or converts approvals into terminal input. Watch remains read-only.
// See docs/pending-input-response.md for protocol and ownership boundaries.
package codexapp

import "encoding/json"

// envelope is the wire shape of every message in both directions: newline-
// delimited JSON-RPC 2.0 (verified empirically against a real `codex
// app-server` process — no Content-Length framing, one JSON object per
// line). A message with a non-empty ID and a Method is a request; one with
// an ID and no Method (or a Result/Error) is a response; one with a Method
// and no ID is a notification.
type envelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Notification/request method names this client recognizes. The full
// protocol has hundreds of methods (see codex app-server generate-json-schema);
// this client only ever looks for the handful relevant to interaction state
// and ignores everything else on the wire.
const (
	methodInitialize          = "initialize"
	methodThreadStarted       = "thread/started"
	methodThreadStatusChanged = "thread/status/changed"

	// Server-to-client requests (approval/input). This client observes them
	// for stable pending-request identity and a privacy-safe summary, but
	// are answered only by the request-bound relay or the real `codex` TUI (the owner
	// connected to the same app-server) is the one actually answering them.
	methodExecCommandApproval      = "execCommandApproval"
	methodApplyPatchApproval       = "applyPatchApproval"
	methodItemCommandExecApproval  = "item/commandExecution/requestApproval"
	methodItemFileChangeApproval   = "item/fileChange/requestApproval"
	methodItemPermissionsApproval  = "item/permissions/requestApproval"
	methodItemToolRequestUserInput = "item/tool/requestUserInput"
)

// initializeParams/clientInfo mirror codex-rs's InitializeParams (verified
// against the real app-server's response to this exact payload).
type initializeParams struct {
	ClientInfo   clientInfo      `json:"clientInfo"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}
type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// threadStatusPayload mirrors ThreadStatus: a tagged union on "type". Only
// "active" carries activeFlags; the others are identified by Type alone.
// See docs/codex-app-server-observer.md for the exact enum values captured
// from a real app-server run (idle observed live; active/waitingOnApproval/
// waitingOnUserInput/notLoaded/systemError taken from the published schema,
// `codex app-server generate-json-schema`, ThreadActiveFlag/ThreadStatus).
type threadStatusPayload struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

const (
	threadStatusNotLoaded   = "notLoaded"
	threadStatusIdle        = "idle"
	threadStatusSystemError = "systemError"
	threadStatusActive      = "active"

	activeFlagWaitingOnApproval  = "waitingOnApproval"
	activeFlagWaitingOnUserInput = "waitingOnUserInput"
)

// threadRef is the minimal subset of the (much larger) Thread object this
// client reads: enough to learn a thread's id and initial status from
// thread/start's result or thread/started's notification.
type threadRef struct {
	ID     string              `json:"id"`
	Status threadStatusPayload `json:"status"`
}

type threadStartedParams struct {
	Thread threadRef `json:"thread"`
}
type threadStatusChangedParams struct {
	ThreadID string              `json:"threadId"`
	Status   threadStatusPayload `json:"status"`
}

// Approval/input request payloads: only the fields this client actually
// uses (stable identity + a privacy-safe, provider-supplied summary). The
// real payloads carry considerably more (full parsed command trees, cwd,
// file diffs); none of that is forwarded anywhere by this client — only a
// short, human-readable summary is derived, matching bridge.PendingRequest's
// privacy contract (never raw output, never a transcript).
type execCommandApprovalParams struct {
	CallID  string   `json:"callId"`
	Command []string `json:"command"`
}
type applyPatchApprovalParams struct {
	CallID      string       `json:"callId"`
	FileChanges []fileChange `json:"fileChanges"`
}
type fileChange struct {
	Path string `json:"path"`
}
type itemApprovalParams struct {
	ItemID  string   `json:"itemId"`
	CallID  string   `json:"callId"`
	Command []string `json:"command"`
}
type toolRequestUserInputParams struct {
	ThreadID  string                         `json:"threadId"`
	ItemID    string                         `json:"itemId"`
	Questions []toolRequestUserInputQuestion `json:"questions"`
}
type toolRequestUserInputQuestion struct {
	ID       string `json:"id"`
	Text     string `json:"question"`
	IsSecret bool   `json:"isSecret"`
}
