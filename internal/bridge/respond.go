package bridge

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrPendingRequestMismatch    = errors.New("pending input request is no longer current")
	ErrRemoteResponseUnsupported = errors.New("provider does not support remote response")
	ErrInvalidResponse           = errors.New("response must be valid nonempty UTF-8 text of at most 16384 bytes without control characters")
)

const MaxResponseBytes = 16384

// PendingInputResponder must revalidate the provider's live request identity.
// Implementations must use a request-bound input operation, never fall back to
// unstructured terminal input, and respect the context deadline.
type PendingInputResponder interface {
	RespondToInput(context.Context, string, string, string) error
}

func ValidateResponse(text string) error {
	if len(text) > MaxResponseBytes || !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return ErrInvalidResponse
	}
	for _, r := range text {
		if (r < 32 && r != '\n' && r != '\t') || r == 127 {
			return ErrInvalidResponse
		}
	}
	return nil
}

// RespondToInput borrows the unowned writer slot for one bounded structured
// operation. Holding the session lock excludes Attach/ClaimWriter/Stop and
// local writes during dispatch. It never evicts a local writer, even one
// controlled by the same user. Provider state remains authoritative afterward.
func (s *Supervisor) RespondToInput(ctx context.Context, sessionID, pendingID, text string) error {
	if err := ValidateResponse(text); err != nil {
		return err
	}
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.recovered {
		return ErrSessionRecoveryUnavailable
	}
	if ms.info.State != SessionStateRunning && ms.info.State != SessionStateAttached {
		return ErrSessionNotRunning
	}
	if ms.info.ActiveWriterClientID != "" {
		return ErrWriterConflict
	}
	i := ms.info.Interaction
	if pendingID == "" || i.State != InteractionWaitingForInput || i.Pending == nil || i.Pending.Type != PendingRequestInput || i.Pending.ID != pendingID {
		return ErrPendingRequestMismatch
	}
	responder, ok := ms.provider.(PendingInputResponder)
	if !ok {
		return ErrRemoteResponseUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return responder.RespondToInput(ctx, sessionID, pendingID, text)
}
