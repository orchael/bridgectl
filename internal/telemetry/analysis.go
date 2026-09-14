package telemetry

import (
	"sort"
	"strings"
	"time"
)

// Turn is a reconstructed contiguous human or agent interaction.
type Turn struct {
	SourceID      string     `json:"source_id,omitempty"`
	SessionID     string     `json:"session_id"`
	ActorID       string     `json:"actor_id,omitempty"`
	Direction     Direction  `json:"direction"`
	Stream        StreamType `json:"stream"`
	SequenceStart uint64     `json:"sequence_start"`
	SequenceEnd   uint64     `json:"sequence_end"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       time.Time  `json:"ended_at"`
	Text          string     `json:"text,omitempty"`
	ByteCount     int        `json:"byte_count"`
	Redactions    int        `json:"redactions"`
}

// DataQuality summarizes evidence integrity observed during reconstruction.
type DataQuality struct {
	Events           int `json:"events"`
	Duplicates       int `json:"duplicates"`
	OutOfOrder       int `json:"out_of_order"`
	MissingSequences int `json:"missing_sequences"`
	LegacyEvents     int `json:"legacy_events"`
	OmittedEvents    int `json:"omitted_events"`
	Redactions       int `json:"redactions"`
}

// InteractionAnalyzer incrementally reconstructs turns without retaining the
// entire raw event corpus.
type InteractionAnalyzer struct {
	lastSequence map[sessionIdentity]uint64
	pending      map[sessionIdentity]Turn
	quality      DataQuality
}

func NewInteractionAnalyzer() *InteractionAnalyzer {
	return &InteractionAnalyzer{lastSequence: make(map[sessionIdentity]uint64), pending: make(map[sessionIdentity]Turn)}
}

// Observe consumes one event and returns any turns completed by that event.
func (a *InteractionAnalyzer) Observe(event Event) []Turn {
	a.quality.Events++
	if event.SchemaVersion == 1 {
		a.quality.LegacyEvents++
	}
	a.quality.Redactions += event.Redactions
	if event.OmittedReason != "" {
		a.quality.OmittedEvents++
	}
	key := eventKey(event)
	last := a.lastSequence[key]
	if event.Sequence > 0 && last > 0 {
		switch {
		case event.Sequence == last:
			a.quality.Duplicates++
			return nil
		case event.Sequence < last:
			a.quality.OutOfOrder++
			return nil
		case event.Sequence > last+1:
			a.quality.MissingSequences += int(event.Sequence - last - 1)
		}
	}
	if event.Sequence > 0 {
		a.lastSequence[key] = event.Sequence
	}
	if event.Kind == EventSessionEnded {
		return a.flush(key)
	}
	if event.Kind != EventProviderOutput && event.Kind != EventUserInput {
		return nil
	}
	pending, ok := a.pending[key]
	if ok && (pending.Direction != event.Direction || pending.Stream != event.Stream) {
		completed := a.flush(key)
		a.startTurn(key, event)
		return completed
	}
	if !ok {
		a.startTurn(key, event)
		return nil
	}
	pending.SequenceEnd = event.Sequence
	pending.EndedAt = event.Timestamp
	pending.Text += event.Text
	pending.ByteCount += event.ByteCount
	pending.Redactions += event.Redactions
	a.pending[key] = pending
	return nil
}

func (a *InteractionAnalyzer) startTurn(key sessionIdentity, event Event) {
	a.pending[key] = Turn{
		SourceID: event.SourceID, SessionID: event.SessionID, ActorID: event.ActorID,
		Direction: event.Direction, Stream: event.Stream, SequenceStart: event.Sequence, SequenceEnd: event.Sequence,
		StartedAt: event.Timestamp, EndedAt: event.Timestamp, Text: event.Text,
		ByteCount: event.ByteCount, Redactions: event.Redactions,
	}
}

func (a *InteractionAnalyzer) flush(key sessionIdentity) []Turn {
	turn, ok := a.pending[key]
	if !ok {
		return nil
	}
	delete(a.pending, key)
	turn.Text = strings.TrimSpace(turn.Text)
	return []Turn{turn}
}

// Finish returns incomplete turns in deterministic composite-identity order.
func (a *InteractionAnalyzer) Finish() []Turn {
	keys := make([]sessionIdentity, 0, len(a.pending))
	for key := range a.pending {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].sourceID == keys[j].sourceID {
			return keys[i].sessionID < keys[j].sessionID
		}
		return keys[i].sourceID < keys[j].sourceID
	})
	var turns []Turn
	for _, key := range keys {
		turns = append(turns, a.flush(key)...)
	}
	return turns
}

func (a *InteractionAnalyzer) Quality() DataQuality { return a.quality }
