package bridgecontrol

import (
	"context"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

// SessionSnapshot is the safe, minimal session metadata bridgecontrol
// publishes to Bridge: no PTY output, environment variables, provider
// credentials, OAuth tokens, or filesystem contents.
type SessionSnapshot struct {
	SessionID string
	Provider  string
	ProjectID string
	Status    string
	CreatedAt time.Time
}

// sessionStatus maps a Supervisor lifecycle state to Bridge's documented
// session status enum (control.validSessionStatuses).
func sessionStatus(state bridge.SessionState) string {
	switch state {
	case bridge.SessionStateStarting:
		return StatusStarting
	case bridge.SessionStateRunning:
		return StatusRunning
	case bridge.SessionStateAttached:
		return StatusAttached
	case bridge.SessionStateStopping:
		return StatusStopping
	case bridge.SessionStateStopped:
		return StatusStopped
	case bridge.SessionStateFailed:
		return StatusFailed
	default:
		return StatusUnknown
	}
}

// ActiveSnapshots filters Supervisor.List's result to non-terminal sessions
// and converts them to the safe metadata sent in an authoritative
// session_snapshot. Terminal sessions (stopped/failed) are excluded: Bridge
// treats any previously-known session absent from a fresh snapshot as
// implicitly stopped, so there is no need to (and no safe way to) keep
// asserting a revision for a session bridgectl no longer tracks.
func ActiveSnapshots(infos []bridge.SessionInfo) []SessionSnapshot {
	out := make([]SessionSnapshot, 0, len(infos))
	for _, info := range infos {
		if info.State == bridge.SessionStateStopped || info.State == bridge.SessionStateFailed {
			continue
		}
		out = append(out, SessionSnapshot{
			SessionID: info.SessionID,
			Provider:  info.Provider,
			ProjectID: info.ProjectID,
			Status:    sessionStatus(info.State),
			CreatedAt: info.CreatedAt,
		})
	}
	return out
}

// SupervisorObserver adapts bridge.Supervisor lifecycle notifications into
// bridgecontrol.Client calls. It implements bridge.ControlObserver.
//
// It never talks to Bridge itself and never blocks: SessionChanged only
// enqueues into Client's bounded, non-blocking notification channel.
type SupervisorObserver struct {
	client *Client
}

// NewSupervisorObserver wraps client as a bridge.ControlObserver.
func NewSupervisorObserver(client *Client) *SupervisorObserver {
	return &SupervisorObserver{client: client}
}

// SessionChanged implements bridge.ControlObserver.
func (o *SupervisorObserver) SessionChanged(info bridge.SessionInfo) {
	o.client.Notify(SessionSnapshot{
		SessionID: info.SessionID,
		Provider:  info.Provider,
		ProjectID: info.ProjectID,
		Status:    sessionStatus(info.State),
		CreatedAt: info.CreatedAt,
	})
}

// Close implements bridge.ControlObserver.
func (o *SupervisorObserver) Close(ctx context.Context) error {
	return o.client.Close(ctx)
}
