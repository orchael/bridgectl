package bridge

import "context"

// ControlObserver receives safe, minimal session lifecycle metadata for an
// optional external presence/coordination system (e.g. a Bridge
// control-plane client). Implementations must never block: notifications are
// expected to be enqueued asynchronously, never delivered synchronously over
// a network connection from inside SessionChanged.
//
// Unlike TelemetryObserver, ControlObserver never sees PTY/stdin byte
// content — only the same safe fields exposed by SessionInfo (session id,
// provider, project id, lifecycle state, interaction state, timestamps).
type ControlObserver interface {
	// SessionChanged is called whenever a session's lifecycle state is set or
	// changes (on start, on attach/detach, on stop, and on process exit) and
	// also whenever its interaction state changes (see
	// Supervisor.UpdateInteraction). info.Interaction always reflects the
	// session's full current interaction state, not just what changed;
	// implementations that only care about one axis can compare against
	// their own last-seen copy of info to tell the two apart.
	SessionChanged(SessionInfo)
	// Close flushes/shuts down the observer. It must return promptly; any
	// network I/O it performs must itself be bounded by ctx.
	Close(context.Context) error
}

// WithControlObserver attaches an optional ControlObserver to the
// Supervisor. It is independent of WithTelemetry: either, both, or neither
// may be configured.
func WithControlObserver(observer ControlObserver) SupervisorOption {
	return func(s *Supervisor) { s.control = observer }
}
