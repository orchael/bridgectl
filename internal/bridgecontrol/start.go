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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type StartRequest struct {
	ID             string    `json:"command_id"`
	OrganizationID string    `json:"organization_id"`
	InstallationID string    `json:"installation_id"`
	UserID         string    `json:"user_id"`
	SessionID      string    `json:"session_id"`
	Directory      string    `json:"directory"`
	Provider       string    `json:"provider"`
	ProjectID      string    `json:"project_id"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// Persist the claim before starting a process. An interrupted or ambiguous
// start is never replayed automatically, including after a daemon restart.
func (c *Client) executeStart(ctx context.Context, p StartRequest) CommandResult {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	result := CommandResult{ID: p.ID, Status: "rejected", Code: "invalid_request"}
	for _, value := range []string{p.ID, p.OrganizationID, p.InstallationID, p.UserID, p.SessionID} {
		if _, err := uuid.Parse(value); err != nil {
			return result
		}
	}
	if p.Directory == "" || len(p.Directory) > 4096 || p.Provider == "" || len(p.Provider) > 128 || p.ProjectID == "" || len(p.ProjectID) > 256 {
		return result
	}
	c.identityMu.Lock()
	org, inst := c.organizationID, c.installationID
	c.identityMu.Unlock()
	if p.OrganizationID != org || p.InstallationID != inst {
		result.Code = "wrong_installation"
		return result
	}
	if c.cfg.StartFunc == nil || c.cfg.CommandPath == "" {
		result.Code = "unsupported"
		return result
	}
	raw, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, []byte(c.cfg.Credential))
	_, _ = mac.Write(raw)
	fingerprint := hex.EncodeToString(mac.Sum(nil))
	path := filepath.Join(c.cfg.CommandPath, p.ID+".json")
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
	deadline, cancel := context.WithDeadline(ctx, p.ExpiresAt)
	if deadline.Err() != nil {
		err = deadline.Err()
	} else {
		err = c.cfg.StartFunc(deadline, p)
	}
	cancel()
	if err == nil {
		result.Status = "accepted"
		result.Code = ""
	} else {
		result = record.Result
		// These are explicit rejections from the local StartSession method.
		// Timeouts, internal errors, and an existing session stay unknown.
		if grpcStatus, ok := status.FromError(err); ok {
			switch grpcStatus.Code() {
			case codes.InvalidArgument:
				result.Status, result.Code = "rejected", "invalid_request"
			case codes.PermissionDenied:
				result.Status, result.Code = "rejected", "permission_denied"
			case codes.ResourceExhausted:
				result.Status, result.Code = "rejected", "resource_limit"
			case codes.FailedPrecondition:
				result.Status, result.Code = "rejected", "setup_failed"
			case codes.Unavailable:
				result.Status, result.Code = "rejected", "server_or_provider_unavailable"
			}
		}
	}
	record.Result = result
	_ = writeCommandRecord(path, record, false)
	return result
}

func (c *Client) stopStartWorkers() {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()
	c.workerStopping = true
	c.workerCancel()
}

func (c *Client) launchStartWorker(work func(context.Context)) bool {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()
	if c.workerStopping {
		return false
	}
	select {
	case c.workerSlots <- struct{}{}:
	default:
		return false
	}
	c.workerWG.Add(1)
	go func() { defer c.workerWG.Done(); defer func() { <-c.workerSlots }(); work(c.workerCtx) }()
	return true
}
