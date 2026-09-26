package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"
)

// Activity is an allowlisted provider event, never a terminal output chunk.
type Activity struct {
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Summary  string    `json:"summary"`
}
type ActivityWindow struct {
	Events               []Activity `json:"events"`
	NextSequence         uint64     `json:"next_sequence"`
	Gap                  bool       `json:"gap"`
	InstructionSupported bool       `json:"instruction_supported"`
}

// ActivityBuffer is local to a provider session, independent of control-plane
// connectivity and telemetry. It retains at most 128 events, 32 KiB, 15 minutes.
// Sequence numbers never reset during this session's lifetime.
type ActivityBuffer struct {
	mu       sync.Mutex
	events   []Activity
	sequence uint64
}

func (b *ActivityBuffer) Append(kind, summary string, now time.Time) {
	if len(kind) > 32 || len(summary) > 512 || !utf8.ValidString(summary) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sequence++
	b.events = append(b.events, Activity{b.sequence, now.UTC(), kind, summary})
	b.trim(now)
}
func (b *ActivityBuffer) trim(now time.Time) {
	for len(b.events) > 0 {
		raw, _ := json.Marshal(b.events)
		if len(b.events) <= 128 && len(raw) <= 32768 && now.Sub(b.events[0].At) <= 15*time.Minute {
			break
		}
		b.events = b.events[1:]
	}
}
func (b *ActivityBuffer) Window(after uint64, maxEvents, maxBytes int, now time.Time) ActivityWindow {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trim(now)
	if maxEvents <= 0 || maxEvents > 128 {
		maxEvents = 128
	}
	if maxBytes < 1024 || maxBytes > 32768 {
		maxBytes = 32768
	}
	w := ActivityWindow{Events: []Activity{}, NextSequence: after}
	oldest := b.sequence + 1
	if len(b.events) > 0 {
		oldest = b.events[0].Sequence
	}
	w.Gap = after < oldest-1 || after > b.sequence
	if w.Gap {
		w.NextSequence = oldest - 1
		after = oldest - 1
	}
	for _, e := range b.events {
		if e.Sequence <= after {
			continue
		}
		previous := w.NextSequence
		w.Events = append(w.Events, e)
		w.NextSequence = e.Sequence
		raw, _ := json.Marshal(w)
		if len(raw) > maxBytes {
			w.Events = w.Events[:len(w.Events)-1]
			w.NextSequence = previous
			break
		}
		if len(w.Events) >= maxEvents {
			break
		}
	}
	return w
}

type ActivityProvider interface {
	ObserveActivity(string, uint64, int, int) (ActivityWindow, error)
}
type InstructionProvider interface {
	SendInstruction(context.Context, string, string) error
}

func (s *Supervisor) ObserveActivity(sessionID string, after uint64, events, bytes int) (ActivityWindow, error) {
	s.mu.RLock()
	ms, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok {
		return ActivityWindow{}, ErrSessionNotFound
	}
	ms.mu.Lock()
	p, ok := ms.provider.(ActivityProvider)
	ms.mu.Unlock()
	if !ok {
		return ActivityWindow{}, ErrRemoteResponseUnsupported
	}
	return p.ObserveActivity(sessionID, after, events, bytes)
}

func (s *Supervisor) SendInstruction(ctx context.Context, sessionID, text string) error {
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
	p, ok := ms.provider.(InstructionProvider)
	if !ok {
		return ErrRemoteResponseUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.SendInstruction(ctx, sessionID, text)
}
