package bridgecontrol

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// StopRequest identifies one graceful stop in an enrolled installation.
type StopRequest struct {
	ID             string    `json:"command_id"`
	OrganizationID string    `json:"organization_id"`
	InstallationID string    `json:"installation_id"`
	UserID         string    `json:"user_id"`
	SessionID      string    `json:"session_id"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func (c *Client) executeStop(ctx context.Context, p StopRequest) CommandResult {
	c.startMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.startMu.Unlock()
		}
	}()
	result := CommandResult{ID: p.ID, Status: "rejected", Code: "invalid_request"}
	for _, value := range []string{p.ID, p.OrganizationID, p.InstallationID, p.UserID, p.SessionID} {
		if _, err := uuid.Parse(value); err != nil {
			return result
		}
	}
	c.identityMu.Lock()
	org, inst := c.organizationID, c.installationID
	c.identityMu.Unlock()
	if p.OrganizationID != org || p.InstallationID != inst {
		result.Code = "wrong_installation"
		return result
	}
	if c.cfg.StopFunc == nil || c.cfg.CommandPath == "" {
		result.Code = "unsupported"
		return result
	}
	raw, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, []byte(c.cfg.Credential))
	_, _ = mac.Write(raw)
	fingerprint := hex.EncodeToString(mac.Sum(nil))
	path := filepath.Join(c.cfg.CommandPath, "stop-"+p.ID+".json")
	if b, err := os.ReadFile(path); err == nil {
		var old commandRecord
		if json.Unmarshal(b, &old) != nil {
			result.Code = "ledger_unavailable"
			return result
		}
		if old.Fingerprint != fingerprint {
			result.Code = "idempotency_conflict"
			return result
		}
		return old.Result
	} else if !errors.Is(err, os.ErrNotExist) {
		result.Code = "ledger_unavailable"
		return result
	}
	if time.Now().After(p.ExpiresAt) || p.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		result.Code = "expired"
		return result
	}
	if os.MkdirAll(c.cfg.CommandPath, 0700) != nil {
		result.Code = "ledger_unavailable"
		return result
	}
	entries, err := os.ReadDir(c.cfg.CommandPath)
	if err != nil || len(entries) >= 10000 {
		result.Code = "ledger_full"
		return result
	}
	record := commandRecord{Fingerprint: fingerprint, Result: CommandResult{ID: p.ID, Status: "unknown", Code: "delivery_unknown"}}
	if ctx.Err() != nil {
		return CommandResult{ID: p.ID, Status: "rejected", Code: "server_or_provider_unavailable"}
	}
	if writeCommandRecord(path, record, true) != nil {
		result.Code = "ledger_unavailable"
		return result
	}
	c.startMu.Unlock()
	locked = false
	deadline, cancel := context.WithDeadline(ctx, p.ExpiresAt)
	if deadline.Err() != nil {
		err = deadline.Err()
	} else {
		err = c.cfg.StopFunc(deadline, p)
	}
	cancel()
	if err == nil {
		result.Status = "accepted"
		result.Code = ""
	} else {
		// A stop may already have reached the supervisor even on error.
		result = record.Result
	}
	record.Result = result
	c.startMu.Lock()
	defer c.startMu.Unlock()
	_ = writeCommandRecord(path, record, false)
	return result
}
