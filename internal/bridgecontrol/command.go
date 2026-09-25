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
	"github.com/orchael/bridgectl/internal/bridge"
)

type Command struct {
	ID               string    `json:"command_id"`
	OrganizationID   string    `json:"organization_id"`
	InstallationID   string    `json:"installation_id"`
	SessionID        string    `json:"session_id"`
	UserID           string    `json:"user_id"`
	Action           string    `json:"action"`
	PendingRequestID string    `json:"pending_request_id"`
	ExpiresAt        time.Time `json:"expires_at"`
	Text             string    `json:"text"`
}
type CommandResult struct {
	ID     string `json:"command_id"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}
type commandRecord struct {
	Fingerprint string        `json:"fingerprint"`
	Result      CommandResult `json:"result"`
}

// executeCommand is serialized by the connection reader. A durable claim is
// synced before dispatch. Unknown outcomes never cause another submission,
// including after restart. The ledger contains metadata/HMAC only, no text.
func (c *Client) executeCommand(ctx context.Context, cmd Command) CommandResult {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	result := CommandResult{ID: cmd.ID, Status: "rejected", Code: "invalid_command"}
	id, err := uuid.Parse(cmd.ID)
	if err != nil || id.String() != cmd.ID || cmd.Action != "respond" || cmd.UserID == "" || len(cmd.UserID) > 256 || cmd.SessionID == "" || len(cmd.SessionID) > 256 || cmd.PendingRequestID == "" || len(cmd.PendingRequestID) > 256 || bridge.ValidateResponse(cmd.Text) != nil {
		return result
	}
	if cmd.OrganizationID != c.organizationID || cmd.InstallationID != c.installationID {
		result.Code = "wrong_installation"
		return result
	}
	if c.cfg.RespondFunc == nil || c.cfg.CommandPath == "" {
		result.Code = "unsupported"
		return result
	}
	body, _ := json.Marshal(cmd)
	mac := hmac.New(sha256.New, []byte(c.cfg.Credential))
	_, _ = mac.Write(body)
	fingerprint := hex.EncodeToString(mac.Sum(nil))
	path := filepath.Join(c.cfg.CommandPath, cmd.ID+".json")
	if raw, err := os.ReadFile(path); err == nil {
		var previous commandRecord
		if json.Unmarshal(raw, &previous) != nil {
			result.Code = "ledger_unavailable"
			return result
		}
		if previous.Fingerprint != fingerprint {
			result.Code = "idempotency_conflict"
			return result
		}
		return previous.Result
	} else if !errors.Is(err, os.ErrNotExist) {
		result.Code = "ledger_unavailable"
		return result
	}
	if time.Now().After(cmd.ExpiresAt) || cmd.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		result.Code = "expired"
		return result
	}
	if err := os.MkdirAll(c.cfg.CommandPath, 0700); err != nil {
		result.Code = "ledger_unavailable"
		return result
	}
	entries, err := os.ReadDir(c.cfg.CommandPath)
	if err != nil || len(entries) >= 10000 {
		result.Code = "ledger_full"
		return result
	}
	record := commandRecord{Fingerprint: fingerprint, Result: CommandResult{ID: cmd.ID, Status: "unknown", Code: "delivery_unknown"}}
	if err := writeCommandRecord(path, record, true); err != nil {
		result.Code = "ledger_unavailable"
		return result
	}
	deadline, cancel := context.WithDeadline(ctx, cmd.ExpiresAt)
	err = c.cfg.RespondFunc(deadline, cmd.SessionID, cmd.PendingRequestID, cmd.Text)
	cancel()
	switch {
	case err == nil:
		result.Status = "accepted"
		result.Code = ""
	case errors.Is(err, bridge.ErrWriterConflict):
		result.Code = "writer_conflict"
	case errors.Is(err, bridge.ErrPendingRequestMismatch):
		result.Code = "pending_request_mismatch"
	case errors.Is(err, bridge.ErrSessionNotFound), errors.Is(err, bridge.ErrSessionNotRunning), errors.Is(err, bridge.ErrSessionRecoveryUnavailable):
		result.Code = "session_unavailable"
	case errors.Is(err, bridge.ErrRemoteResponseUnsupported):
		result.Code = "unsupported"
	default:
		result = record.Result
	}
	record.Result = result
	// If persistence fails, the on-disk unknown claim still prevents replay.
	_ = writeCommandRecord(path, record, false)
	return result
}

func writeCommandRecord(path string, record commandRecord, claim bool) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	target := path
	if !claim {
		target = path + ".tmp"
	}
	file, err := os.OpenFile(target, flags, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if !claim {
		if err = os.Rename(target, path); err != nil {
			return err
		}
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
