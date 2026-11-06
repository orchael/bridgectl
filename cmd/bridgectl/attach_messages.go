package main

import (
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

// User-facing writer messages match the Bridge web wording. They never
// contain gRPC codes or client IDs; IDs are logged at debug level.
const (
	msgYouControl     = "You now control this session."
	msgYouLostControl = "Another client took control. You are watching."
	msgOtherControls  = "Another client now controls this session."
	msgControlFreed   = "No client controls this session now."
)

// isWriterConflict reports whether an attach failed because another client
// holds the single writer slot (server ErrWriterConflict → AlreadyExists).
func isWriterConflict(err error) bool {
	return status.Code(err) == codes.AlreadyExists || errors.Is(err, bridgeclient.ErrSessionAlreadyExists)
}

// writerConflictError explains how to proceed when the writer slot is taken.
// The server does not report what kind of client holds it, so the message
// does not guess.
func writerConflictError(sessionID, remote string) error {
	cmd := "bridgectl session attach"
	if remote != "" {
		cmd += " --remote " + remote
	}
	return writerConflict(fmt.Sprintf("Another client is controlling this session.\n"+
		"Watch instead:   %s --observe %s\n"+
		"Take control:    %s --take-over %s", cmd, sessionID, cmd, sessionID))
}

// writerConflict is a complete, user-facing sentence printed as-is by main.
type writerConflict string

func (e writerConflict) Error() string { return string(e) }

// attachStreamError maps the error that ended an attach stream before the
// server acknowledged it.
func attachStreamError(err error, sessionID, remote string) error {
	if isWriterConflict(err) {
		slog.Debug("attach rejected: writer conflict", "session_id", sessionID, "error", err)
		return writerConflictError(sessionID, remote)
	}
	return fmt.Errorf("attach: %w", err)
}

// writerTracker turns WRITER_CLAIMED/WRITER_RELEASED events into plain
// messages from this client's point of view.
type writerTracker struct {
	self string
	// controlling is true while this client holds the writer slot.
	controlling bool
	// claiming suppresses the release of the previous writer that precedes
	// our own forced claim.
	claiming bool
	// lost is set when our slot was released by someone else's claim.
	lost bool
}

func (w *writerTracker) message(ev *bridgev1.AttachSessionEvent) string {
	id := ev.GetWriterClientId()
	switch ev.GetType() {
	case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_CLAIMED:
		slog.Debug("writer claimed", "session_id", ev.GetSessionId(), "client_id", id, "self", id == w.self)
		if id == w.self {
			w.controlling, w.claiming, w.lost = true, false, false
			return msgYouControl
		}
		wasOurs := w.controlling || w.lost
		w.controlling, w.lost = false, false
		if wasOurs {
			return msgYouLostControl
		}
		return msgOtherControls
	case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED:
		slog.Debug("writer released", "session_id", ev.GetSessionId(), "client_id", id, "self", id == w.self)
		if id == w.self {
			// Either our own detach, or a take-over whose claim follows.
			if w.controlling {
				w.controlling, w.lost = false, true
			}
			return ""
		}
		if w.claiming {
			return ""
		}
		return msgControlFreed
	}
	return ""
}
