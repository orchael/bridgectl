package bridgecontrol

import (
	"encoding/json"
	"errors"

	"github.com/orchael/bridgectl/internal/bridge"
)

// maxDiagnosticBytes bounds the diagnostic report accepted from DiagnoseFunc.
// The schema-v1 report is a few hundred bytes; anything near this limit is not
// a diagnostic and must not be sent to Bridge.
const maxDiagnosticBytes = 16 * 1024

// DiagnosticRequest asks bridgectl for the schema-versioned diagnostic report
// of one session ("diagnose_session"). It carries identity only: Bridge
// cannot pass options, paths or commands, and bridgectl answers from its own
// Supervisor state and local control files, never from Bridge.
type DiagnosticRequest struct {
	ID             string `json:"request_id"`
	OrganizationID string `json:"organization_id"`
	InstallationID string `json:"installation_id"`
	SessionID      string `json:"session_id"`
}

// DiagnosticResult is the "session_diagnostic" reply. Report is the canonical
// diagnostic document (internal/diagnose.Report) as opaque JSON so this
// package, which diagnose imports, does not depend on it. Code is a fixed
// vocabulary: "" on success, otherwise invalid_request, unsupported,
// not_found or unavailable.
type DiagnosticResult struct {
	ID     string          `json:"request_id"`
	Report json.RawMessage `json:"report,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// diagnose answers a diagnose_session request. Requests for another
// organization or installation are rejected before DiagnoseFunc runs, like
// observe; errors from DiagnoseFunc surface only as a fixed code, never as
// error text, which could carry provider output.
func (c *Client) diagnose(r DiagnosticRequest) DiagnosticResult {
	result := DiagnosticResult{ID: r.ID, Code: "unsupported"}
	c.identityMu.Lock()
	organizationID, installationID := c.organizationID, c.installationID
	c.identityMu.Unlock()
	if r.ID == "" || len(r.ID) > 128 || r.SessionID == "" || len(r.SessionID) > 256 || r.OrganizationID != organizationID || r.InstallationID != installationID {
		result.Code = "invalid_request"
		return result
	}
	if c.cfg.DiagnoseFunc == nil {
		return result
	}
	report, err := c.cfg.DiagnoseFunc(r.SessionID)
	switch {
	case errors.Is(err, bridge.ErrSessionNotFound):
		result.Code = "not_found"
	case err != nil:
		result.Code = "unavailable"
	case len(report) == 0 || len(report) > maxDiagnosticBytes || !json.Valid(report):
		result.Code = "unavailable"
	default:
		result.Report = report
		result.Code = ""
	}
	return result
}
