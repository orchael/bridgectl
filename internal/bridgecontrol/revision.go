package bridgecontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// RevisionStore persists the monotonic per-session lifecycle revision
// counters bridgectl has assigned when publishing to Bridge. Bridge requires
// a strictly increasing revision per session_id (session_started must start
// at 1; each later event must be exactly current+1) and uses it to detect
// duplicates, stale events, and gaps.
//
// Revisions are scoped to (session_id) only, matching Bridge's contract, and
// are assigned lazily at send time (see Client.sendEvent), not at the
// moment a local state change occurs — so a notification dropped from the
// bounded in-memory queue never creates a numeric gap on the wire, only a
// delay in Bridge learning about an intermediate state.
//
// Persisting to disk (rather than keeping this purely in memory) matters
// when the bridgectl daemon itself restarts while a session is recovered
// (SessionInfo.Recovered): without it, a fresh in-memory counter would
// reissue revision 1 for an already-known session, and Bridge would ignore
// that snapshot row as stale (a lower revision than what it already has),
// silently failing to reconcile the session's true current status.
type RevisionStore struct {
	mu   sync.Mutex
	path string
	rev  map[string]int64
}

// NewRevisionStore loads persisted revisions from path if present. A
// missing or unreadable file is treated as an empty store (fresh
// installation, or first run after this feature was added) rather than an
// error: revision bookkeeping is best-effort recovery metadata, not
// something whose absence should block the control client from starting.
func NewRevisionStore(path string) *RevisionStore {
	rs := &RevisionStore{path: path, rev: make(map[string]int64)}
	if path == "" {
		return rs
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return rs
	}
	var loaded map[string]int64
	if err := json.Unmarshal(b, &loaded); err == nil {
		rs.rev = loaded
	}
	return rs
}

// Next allocates and persists the next monotonic revision for sessionID,
// starting at 1 for a session never seen before.
func (rs *RevisionStore) Next(sessionID string) int64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	next := rs.rev[sessionID] + 1
	rs.rev[sessionID] = next
	rs.persistLocked()
	return next
}

// Current returns the last revision assigned to sessionID, or 0 if none has
// been assigned yet.
func (rs *RevisionStore) Current(sessionID string) int64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.rev[sessionID]
}

// Forget drops bookkeeping for a session that has reached a terminal state
// and been successfully reported to Bridge, keeping this store bounded to
// currently-relevant sessions rather than growing for the lifetime of the
// daemon process.
func (rs *RevisionStore) Forget(sessionID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, ok := rs.rev[sessionID]; !ok {
		return
	}
	delete(rs.rev, sessionID)
	rs.persistLocked()
}

// persistLocked writes the current revision map to disk atomically. Errors
// are silently ignored (matching NewRevisionStore's best-effort read): a
// failure to persist only risks re-deriving a slightly stale revision after
// an unlikely daemon-restart-during-recovery race, never data loss or a
// blocked local operation.
func (rs *RevisionStore) persistLocked() {
	if rs.path == "" {
		return
	}
	b, err := json.Marshal(rs.rev)
	if err != nil {
		return
	}
	dir := filepath.Dir(rs.path)
	tmp, err := os.CreateTemp(dir, ".bridge-control-revisions-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	_ = os.Rename(tmpPath, rs.path)
}
