package bridgecontrol

import "github.com/orchael/bridgectl/internal/bridge"

type ObservationRequest struct {
	ID             string `json:"request_id"`
	OrganizationID string `json:"organization_id"`
	InstallationID string `json:"installation_id"`
	SessionID      string `json:"session_id"`
	AfterSequence  uint64 `json:"after_sequence"`
	MaxEvents      int    `json:"max_events"`
	MaxBytes       int    `json:"max_bytes"`
}
type ObservationResult struct {
	ID     string                `json:"request_id"`
	Window bridge.ActivityWindow `json:"window"`
	Code   string                `json:"code,omitempty"`
}

func (c *Client) observe(r ObservationRequest) ObservationResult {
	result := ObservationResult{ID: r.ID, Code: "unsupported"}
	if r.ID == "" || len(r.ID) > 128 || r.SessionID == "" || len(r.SessionID) > 256 || r.OrganizationID != c.organizationID || r.InstallationID != c.installationID {
		result.Code = "invalid_request"
		return result
	}
	if c.cfg.ObserveFunc == nil {
		return result
	}
	w, err := c.cfg.ObserveFunc(r.SessionID, r.AfterSequence, r.MaxEvents, r.MaxBytes)
	if err != nil {
		return result
	}
	result.Window = w
	result.Code = ""
	return result
}
