package bridgecontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State summarizes the control client's current connection state for
// display in `bridgectl doctor`. It is persisted to a small local file
// rather than queried live, because `doctor` runs as a short-lived CLI
// process while the control connection is held by the long-lived daemon;
// a live check would either require RPC plumbing or, if it opened its own
// WebSocket with the shared installation credential, would trigger Bridge's
// generation-fencing and evict the daemon's real connection.
type State string

const (
	// StateNotProvisioned means the current enrollment has no control
	// endpoint/credential (pre-Bridge-#16 enrollment, or logged out).
	StateNotProvisioned State = "not_provisioned"
	StateConnecting     State = "connecting"
	StateConnected      State = "connected"
	StateDisconnected   State = "disconnected"
	StateAuthRejected   State = "auth_rejected"
	StateUnavailable    State = "unavailable"
)

// Status is the JSON shape persisted to disk and read back by `doctor`.
type Status struct {
	State          State     `json:"state"`
	InstallationID string    `json:"installation_id,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// WriteStatus atomically persists st to path. Errors are the caller's to
// handle (typically logged, never fatal): status reporting is best-effort
// observability, not part of the control protocol itself.
func WriteStatus(path string, st Status) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bridge-control-status-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// ReadStatus reads back a previously persisted Status. Returns
// os.ErrNotExist (wrapped) when the daemon has never started a control
// client, which the caller should treat the same as "not provisioned" or
// "not yet connected" depending on context.
func ReadStatus(path string) (*Status, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}
